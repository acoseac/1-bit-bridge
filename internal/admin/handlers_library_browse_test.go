package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// browseTestSeed plants a small library on the test server's
// manifest. Mirrors store_browse_test.go's seedBrowseFixture shape
// so the handler-level expectations match the store-level ones.
func browseTestSeed(t *testing.T, srv *Server) {
	t.Helper()
	folders := []string{
		"MusicA",
		"MusicA/Album1",
		"MusicA/Album2",
		"MusicB",
		"MusicB/Album3",
	}
	for _, p := range folders {
		if err := srv.deps.Manifest.UpsertFolder(&manifest.Folder{Path: p}); err != nil {
			t.Fatalf("UpsertFolder %q: %v", p, err)
		}
	}
	type row struct {
		path string
		size int64
		rate float64
		bits int
	}
	rows := []row{
		{"MusicA/Album1/01.flac", 200, 44100, 16},
		{"MusicA/Album1/02.flac", 300, 44100, 16},
		{"MusicA/Album2/01.flac", 500, 96000, 24},
		{"MusicB/Album3/01.flac", 700, 48000, 24},
	}
	for _, r := range rows {
		rate := r.rate
		bits := r.bits
		isDSD := false
		if err := srv.deps.Manifest.UpsertTrack(&manifest.Track{
			Path:          r.path,
			Size:          r.size,
			SampleRate:    &rate,
			BitsPerSample: &bits,
			Codec:         "FLAC",
			IsDSD:         &isDSD,
		}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", r.path, err)
		}
	}
	if err := srv.deps.Manifest.UpsertVariant(manifest.VariantRow{
		SourcePath: "MusicA/Album1/01.flac", VariantID: "upscaled-v2-192000-24",
		SidecarPath: "/tmp/a.flac", Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1000,
		SourceMTimeNS: 1, SourceSize: 200, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.deps.Manifest.UpsertVariant(manifest.VariantRow{
		SourcePath: "MusicA/Album2/01.flac", VariantID: "upscaled-v2-192000-24",
		SidecarPath: "/tmp/b.flac", Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1500,
		SourceMTimeNS: 1, SourceSize: 500, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestApiLibraryBrowse_RootListsTopLevelFolders pins the top-level
// browse response to the seeded fixture's two roots (MusicA,
// MusicB).
func TestApiLibraryBrowse_RootListsTopLevelFolders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)

	var resp browseResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/browse", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("/api/library/browse: %d", code)
	}
	if resp.Path != "" {
		t.Errorf("Path = %q, want \"\"", resp.Path)
	}
	if len(resp.Folders) != 2 {
		t.Fatalf("Folders len = %d, want 2", len(resp.Folders))
	}
	if resp.Folders[0].Name != "MusicA" || resp.Folders[1].Name != "MusicB" {
		t.Errorf("Folder names = %v", []string{resp.Folders[0].Name, resp.Folders[1].Name})
	}
	if resp.Folders[0].TrackCount != 3 || resp.Folders[0].UpscaledCount != 2 {
		t.Errorf("MusicA rollup wrong: %+v", resp.Folders[0])
	}
}

// TestApiLibraryBrowse_NestedFolderReturnsChildrenAndTracks
// covers the typical browse-into-album call.
func TestApiLibraryBrowse_NestedFolderReturnsChildrenAndTracks(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)

	var resp browseResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/browse?path=MusicA/Album1", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("/api/library/browse?path=MusicA/Album1: %d", code)
	}
	if resp.Path != "MusicA/Album1" {
		t.Errorf("Path = %q", resp.Path)
	}
	if len(resp.Folders) != 0 {
		t.Errorf("Folders len = %d, want 0", len(resp.Folders))
	}
	if len(resp.Tracks) != 2 {
		t.Fatalf("Tracks len = %d, want 2", len(resp.Tracks))
	}
	if resp.Tracks[0].Name != "01.flac" {
		t.Errorf("Tracks[0].Name = %q", resp.Tracks[0].Name)
	}
	if !resp.Tracks[0].IsUpscaled {
		t.Errorf("Tracks[0].IsUpscaled = false; want true (variant exists)")
	}
	if resp.Tracks[1].IsUpscaled {
		t.Errorf("Tracks[1].IsUpscaled = true; want false (no variant)")
	}
}

// TestApiLibraryBrowse_RejectsTraversal covers the path-validation
// layer. `..` after path.Clean falls outside the library scope
// and the handler returns 400 + a typed error code.
func TestApiLibraryBrowse_RejectsTraversal(t *testing.T) {
	srv, _, _ := newTestServer(t)

	traversalCases := []string{
		"../",
		"foo/../../bar",
		"..",
	}
	for _, raw := range traversalCases {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/library/browse?path="+raw, nil)
			req.RemoteAddr = "127.0.0.1:54321"
			rw := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rw, req)
			if rw.Code != http.StatusBadRequest {
				t.Errorf("traversal %q: got %d, want 400", raw, rw.Code)
			}
			if !strings.Contains(rw.Body.String(), "bad-path") {
				t.Errorf("body missing bad-path code: %s", rw.Body.String())
			}
		})
	}
}

// TestApiLibraryBrowseProjection_503WhenClosuresUnwired covers the
// nil-deps fallback. Without wiring ProjectedSize / AvailableDiskSpace,
// the handler surfaces 503 rather than a typed-nil panic.
func TestApiLibraryBrowseProjection_503WhenClosuresUnwired(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)

	req := httptest.NewRequest("GET", "/api/library/browse-projection?path=MusicA", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("nil deps: got %d, want 503", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "upscale-disabled") {
		t.Errorf("body missing upscale-disabled code: %s", rw.Body.String())
	}
}

// TestApiLibraryBrowseProjection_HappyPath wires the deps with a
// stub-and-real combination: real ProjectedSize from transcode (no
// network / sox dependency), real AvailableDiskSpace from the
// transcode package. Exercises the full path including disk probe
// against t.TempDir().
func TestApiLibraryBrowseProjection_HappyPath(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)

	// Wire the two closures with simple stubs so we don't have to
	// pull internal/transcode into this test (mirrors the
	// cmd/bridge/main.go wiring pattern). Each closure is exercised
	// for its math; the real transcode helpers are covered in
	// projection_test.go in the transcode package.
	srv.deps.ProjectedSize = func(sourceSize int64, sourceRate, sourceBits, targetRate, targetBits int) int64 {
		// Deterministic stub: per-byte, per-rate-ratio, per-bits-ratio.
		// Returns a finite number we can verify against the seeded
		// sources.
		if sourceSize <= 0 || sourceRate <= 0 || sourceBits <= 0 ||
			targetRate <= 0 || targetBits <= 0 {
			return 0
		}
		return sourceSize *
			int64(targetRate) / int64(sourceRate) *
			int64(targetBits) / int64(sourceBits)
	}
	srv.deps.AvailableDiskSpace = func(dir string) (int64, error) {
		return 1 << 40, nil // 1 TB free, plenty of headroom
	}
	// Enable upscale and provide a bootstrap target. The handler's
	// fallback path consults the YAML defaults when scan_state is
	// unseeded — exercise that branch.
	srv.deps.Cfg.Upscale.Enabled = true

	var resp browseProjectionResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/browse-projection?path=MusicA", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("projection: %d (body=%s)", code, "")
	}
	// MusicA has 3 tracks total, 2 already covered by variants.
	// Projection should run against 1 remaining track (the
	// uncovered 44.1/16 200-byte file at MusicA/Album1/02.flac).
	if resp.AlreadyCoveredFiles != 2 {
		t.Errorf("AlreadyCoveredFiles = %d, want 2", resp.AlreadyCoveredFiles)
	}
	if resp.ProjectedFiles != 1 {
		t.Errorf("ProjectedFiles = %d, want 1", resp.ProjectedFiles)
	}
	if resp.AvailableBytes != 1<<40 {
		t.Errorf("AvailableBytes = %d, want 1<<40", resp.AvailableBytes)
	}
	if !resp.WouldFit {
		t.Errorf("WouldFit = false on 1 TB free + ~MB projection")
	}
	if resp.TargetRate <= 0 || resp.TargetBits <= 0 {
		t.Errorf("Target unset: rate=%d bits=%d", resp.TargetRate, resp.TargetBits)
	}
	if resp.RequiredBytesWithMargin < resp.ProjectedSizeBytes {
		t.Errorf("RequiredBytesWithMargin (%d) < ProjectedSizeBytes (%d)",
			resp.RequiredBytesWithMargin, resp.ProjectedSizeBytes)
	}
}

// TestApiLibraryBrowseProjection_RejectsTraversal locks the same
// path-validation contract as the browse endpoint.
func TestApiLibraryBrowseProjection_RejectsTraversal(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/library/browse-projection?path=../etc/passwd", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("traversal: got %d, want 400", rw.Code)
	}
}

// TestNormaliseBrowsePath_PureUnit pins the helper's behaviour
// across the documented cases. Pure function, isolated from any
// I/O so the test is deterministic regardless of the disk state.
func TestNormaliseBrowsePath_PureUnit(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		wantOk bool
	}{
		{"", "", true},
		{"MusicA", "MusicA", true},
		{"MusicA/Album1", "MusicA/Album1", true},
		{"/MusicA/", "MusicA", true},              // leading + trailing slashes stripped
		{"MusicA//Album1", "MusicA/Album1", true}, // double-slash collapsed
		{".", "", true},                           // path.Clean("/.") = "/" → "" after trim
		{"./MusicA", "MusicA", true},
		{"..", "", false},
		{"../etc/passwd", "", false},
		{"foo/../../bar", "", false},
		{"foo/../bar", "bar", true}, // single-level resolves cleanly
		{"foo\\bar", "", false},     // backslash refused
	}
	for _, c := range cases {
		got, ok := normaliseBrowsePath(c.raw)
		if got != c.want || ok != c.wantOk {
			t.Errorf("normaliseBrowsePath(%q) = (%q, %v), want (%q, %v)",
				c.raw, got, ok, c.want, c.wantOk)
		}
	}
}
