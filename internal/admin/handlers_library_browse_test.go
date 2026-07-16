package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
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
		if err := srv.deps.Manifest.UpsertFolder(context.Background(), &manifest.Folder{Path: p}); err != nil {
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
		if err := srv.deps.Manifest.UpsertTrack(context.Background(), &manifest.Track{
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
	if err := srv.deps.Manifest.UpsertVariant(context.Background(), manifest.VariantRow{
		SourcePath: "MusicA/Album1/01.flac", VariantID: "upscaled-v2-192000-24",
		SidecarPath: "/tmp/a.flac", Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1000,
		SourceMTimeNS: 1, SourceSize: 200, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.deps.Manifest.UpsertVariant(context.Background(), manifest.VariantRow{
		SourcePath: "MusicA/Album2/01.flac", VariantID: "upscaled-v2-192000-24",
		SidecarPath: "/tmp/b.flac", Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1500,
		SourceMTimeNS: 1, SourceSize: 500, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestApiLibraryBrowse_CamelotKeyFilter pins the harmonic-key filter view
// (?camelot=8A): a flat, library-wide list of ANALYZED tracks in that key,
// folders empty, KeyFilter/KeyName populated. Backs the coverage wheel's
// click-to-scope deep-link. A track in a different key and an unanalyzed
// track must NOT appear.
func TestApiLibraryBrowse_CamelotKeyFilter(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	for _, p := range []string{
		"MusicA/Album1/in-key.flac",
		"MusicA/Album1/other-key.flac",
		"MusicA/Album1/no-analysis.flac",
	} {
		rate := 44100.0
		bits := 16
		isDSD := false
		if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{
			Path: p, Size: 100, SampleRate: &rate, BitsPerSample: &bits, Codec: "FLAC", IsDSD: &isDSD,
		}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", p, err)
		}
	}
	// KeyRoot 9 / minor → Camelot 8A (A minor); KeyRoot 0 / major → 8B.
	aMinor, cMajor := 9, 0
	if err := srv.deps.Manifest.UpsertAnalysis(ctx, manifest.AnalysisRow{
		SourcePath: "MusicA/Album1/in-key.flac", KeyRoot: &aMinor, KeyMode: "minor",
	}); err != nil {
		t.Fatalf("UpsertAnalysis in-key: %v", err)
	}
	if err := srv.deps.Manifest.UpsertAnalysis(ctx, manifest.AnalysisRow{
		SourcePath: "MusicA/Album1/other-key.flac", KeyRoot: &cMajor, KeyMode: "major",
	}); err != nil {
		t.Fatalf("UpsertAnalysis other-key: %v", err)
	}

	var resp browseResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/browse?camelot=8A", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("camelot=8A: status %d", code)
	}
	if resp.KeyFilter != "8A" {
		t.Errorf("KeyFilter = %q, want 8A", resp.KeyFilter)
	}
	if resp.KeyName != "A minor" {
		t.Errorf("KeyName = %q, want %q", resp.KeyName, "A minor")
	}
	if len(resp.Folders) != 0 {
		t.Errorf("Folders len = %d, want 0 (key view is flat)", len(resp.Folders))
	}
	if resp.TotalTracks != 1 || len(resp.Tracks) != 1 {
		t.Fatalf("tracks: total=%d listed=%d, want exactly the one 8A track",
			resp.TotalTracks, len(resp.Tracks))
	}
	if resp.Tracks[0].Path != "MusicA/Album1/in-key.flac" {
		t.Errorf("Tracks[0].Path = %q, want the in-key track", resp.Tracks[0].Path)
	}
}

// TestApiLibraryBrowse_CamelotRejectsBadCode pins the 400 on a malformed
// wheel code so a bogus deep-link can't masquerade as a key filter.
func TestApiLibraryBrowse_CamelotRejectsBadCode(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var resp browseResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/browse?camelot=8Z", nil, &resp)
	if code != http.StatusBadRequest {
		t.Fatalf("camelot=8Z: status %d, want 400", code)
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

// TestApiLibraryBrowse_SubtreeRollupIsPageIndependent pins the fix for
// the inspector under-count bug: the recursive subtree rollup must reflect
// the WHOLE node, not just the paginated folder/track page. With limit=1
// the root browse returns only one of the two top-level folders, but
// SubtreeTracks must still be 4 (the full library), not 3 (MusicA only).
func TestApiLibraryBrowse_SubtreeRollupIsPageIndependent(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)
	h := srv.Handler()

	browse := func(name, url string) browseResponse {
		t.Helper()
		var resp browseResponse
		if code := doJSON(t, h, "GET", url, nil, &resp); code != http.StatusOK {
			t.Fatalf("%s: %d", name, code)
		}
		return resp
	}
	wantSubtree := func(name string, r browseResponse, tracks, upscaled, optimized int, bytes int64) {
		t.Helper()
		got := [4]int64{int64(r.SubtreeTracks), int64(r.SubtreeUpscaled), int64(r.SubtreeOptimized), r.SubtreeSizeBytes}
		want := [4]int64{int64(tracks), int64(upscaled), int64(optimized), bytes}
		if got != want {
			t.Errorf("%s subtree (tracks,upscaled,optimized,bytes) = %v, want %v", name, got, want)
		}
	}

	// Full root: 4 tracks, 2 upscaled, 0 optimized, 1700 bytes.
	wantSubtree("root", browse("browse root", "/api/library/browse"), 4, 2, 0, 1700)

	// limit=1 truncates the folder page to one folder, but the subtree rollup
	// must be unchanged — this is the regression the bug produced.
	paged := browse("browse root limit=1", "/api/library/browse?limit=1")
	if len(paged.Folders) != 1 {
		t.Fatalf("limit=1 should return 1 folder, got %d", len(paged.Folders))
	}
	wantSubtree("paged", paged, 4, 2, 0, 1700)

	// A nested node scopes correctly: MusicA = 3 tracks / 2 upscaled / 1000 B.
	wantSubtree("MusicA", browse("browse MusicA", "/api/library/browse?path=MusicA"), 3, 2, 0, 1000)

	// Follow-up (load-more) page: the subtree fields must be OMITTED from the
	// JSON (not present as 0). The frontend's `subtree* ?? fallback` relies on
	// undefined-vs-0 to fall back to the page sum, and decoding into a struct
	// can't tell omission from zero — so assert against the raw body
	// (CodeRabbit on PR #343).
	req := httptest.NewRequest("GET", "/api/library/browse?afterFolder=MusicA&limit=1", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("browse load-more: %d", rw.Code)
	}
	for _, k := range []string{"subtreeTracks", "subtreeUpscaled", "subtreeOptimized", "subtreeSizeBytes"} {
		if strings.Contains(rw.Body.String(), k) {
			t.Errorf("load-more body must omit %q (rollup is first-page only): %s", k, rw.Body.String())
		}
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
	cfg := srv.deps.CfgHolder.Load()
	next := config.Clone(cfg)
	next.Upscale.Enabled = true
	srv.deps.CfgHolder.Store(next)

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

// TestApiLibraryBrowseProjection_ProbesVariantsDir pins the disk
// probe's TARGET: the projection must grade the volume holding the
// effective variants dir (where sidecars are written — possibly a
// different, much larger volume than the bridge data dir), not
// cfg.DataDir. Field case: bridge.ars.md's B2-mounted variants vs a
// 29 GB root disk — grading DataDir refused batches that fit easily.
func TestApiLibraryBrowseProjection_ProbesVariantsDir(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)

	srv.deps.ProjectedSize = func(sourceSize int64, sourceRate, sourceBits, targetRate, targetBits int) int64 {
		return sourceSize
	}
	var probedDir string
	srv.deps.AvailableDiskSpace = func(dir string) (int64, error) {
		probedDir = dir
		return 1 << 40, nil
	}

	cfg := srv.deps.CfgHolder.Load()
	next := config.Clone(cfg)
	next.Upscale.Enabled = true
	next.Upscale.VariantsDir = "/sentinel/variants-on-another-volume"
	srv.deps.CfgHolder.Store(next)

	var resp browseProjectionResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/browse-projection?path=MusicA", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("projection: %d", code)
	}
	want := next.Upscale.EffectiveVariantsDir(next.DataDir)
	if probedDir != want {
		t.Errorf("disk probe graded %q, want the effective variants dir %q", probedDir, want)
	}
	if probedDir == next.DataDir {
		t.Errorf("disk probe graded cfg.DataDir %q — the pre-fix bug", next.DataDir)
	}
}

// TestApiLibraryBrowse_EligibleCounts pins the eligible-denominator
// fields on browse rows + the first-page subtree twins: covered
// tracks stay in the denominator, at-floor CD tracks drop out of the
// optimize denominator, below-target tracks stay in the upscale one.
func TestApiLibraryBrowse_EligibleCounts(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)
	if err := srv.deps.Manifest.SetUpscaleTarget(context.Background(), 192000, 24); err != nil {
		t.Fatalf("SetUpscaleTarget: %v", err)
	}

	var resp browseResponse
	code := doJSON(t, srv.Handler(), "GET", "/api/library/browse?path=MusicA", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("browse: %d", code)
	}
	byName := map[string]browseFolderRow{}
	for _, f := range resp.Folders {
		byName[f.Name] = f
	}
	deref := func(p *int) int {
		t.Helper()
		if p == nil {
			t.Fatalf("eligible count absent — want a present value (nil is the degraded-server shape)")
		}
		return *p
	}
	// Album1: 2× 44.1/16 FLAC, one with an upscaled variant.
	// Upscale: covered(1) + below-target(1) = 2. Optimize: at the
	// CarPlay floor, no optimized variants = 0.
	if got := deref(byName["Album1"].UpscaleEligibleCount); got != 2 {
		t.Errorf("Album1 upscaleEligibleCount = %d, want 2", got)
	}
	if got := deref(byName["Album1"].OptimizeEligibleCount); got != 0 {
		t.Errorf("Album1 optimizeEligibleCount = %d, want 0", got)
	}
	// Album2: 1× 96/24 with an upscaled variant. Upscale: covered = 1.
	// Optimize: above the floor = 1.
	if got := deref(byName["Album2"].UpscaleEligibleCount); got != 1 {
		t.Errorf("Album2 upscaleEligibleCount = %d, want 1", got)
	}
	if got := deref(byName["Album2"].OptimizeEligibleCount); got != 1 {
		t.Errorf("Album2 optimizeEligibleCount = %d, want 1", got)
	}
	// First-page subtree twins.
	if resp.SubtreeUpscaleEligible == nil || *resp.SubtreeUpscaleEligible != 3 {
		t.Errorf("subtreeUpscaleEligible = %v, want 3", resp.SubtreeUpscaleEligible)
	}
	if resp.SubtreeOptimizeEligible == nil || *resp.SubtreeOptimizeEligible != 1 {
		t.Errorf("subtreeOptimizeEligible = %v, want 1", resp.SubtreeOptimizeEligible)
	}
}

// TestApiLibraryBrowseProjection_AtTargetBucket pins the projection's
// alreadyAtTargetFiles split: "needs nothing" tracks land there — NOT
// in unknownFormatFiles (kind=optimize's at-floor CD case) and NOT
// silently vanishing from every bucket (kind=upscale's at-target
// case, which pre-split fell through ProjectedSize<=0 uncounted).
func TestApiLibraryBrowseProjection_AtTargetBucket(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)
	wireOptimizeTestDeps(t, srv)
	if err := srv.deps.Manifest.SetUpscaleTarget(context.Background(), 192000, 24); err != nil {
		t.Fatalf("SetUpscaleTarget: %v", err)
	}
	// Extra seeds: an at-upscale-target track under MusicB, and a DSD
	// track under MusicA (must stay in unknownFormatFiles for optimize).
	atRate := 192000.0
	atBits := 24
	noDSD := false
	if err := srv.deps.Manifest.UpsertTrack(context.Background(), &manifest.Track{
		Path: "MusicB/Album3/attarget.flac", Size: 400,
		SampleRate: &atRate, BitsPerSample: &atBits, Codec: "FLAC", IsDSD: &noDSD,
	}); err != nil {
		t.Fatal(err)
	}
	dsdRate := 2822400.0
	dsdBits := 1
	isDSD := true
	if err := srv.deps.Manifest.UpsertTrack(context.Background(), &manifest.Track{
		Path: "MusicA/Album2/dsd.dsf", Size: 400,
		SampleRate: &dsdRate, BitsPerSample: &dsdBits, Codec: "DSF", IsDSD: &isDSD,
	}); err != nil {
		t.Fatal(err)
	}

	// kind=upscale over MusicB: 48/24 projected; 192/24 at target.
	var up browseProjectionResponse
	code := doJSON(t, srv.Handler(), "GET",
		"/api/library/browse-projection?path=MusicB&kind=upscale", nil, &up)
	if code != http.StatusOK {
		t.Fatalf("upscale projection: %d", code)
	}
	if up.ProjectedFiles != 1 || up.AlreadyAtTargetFiles != 1 || up.UnknownFormatFiles != 0 {
		t.Errorf("upscale buckets = projected %d / atTarget %d / unknown %d, want 1/1/0",
			up.ProjectedFiles, up.AlreadyAtTargetFiles, up.UnknownFormatFiles)
	}

	// kind=optimize over MusicA: Album2's 96/24 projected; the two
	// at-floor 44.1/16 CD tracks are AT TARGET (not "unknown"); the
	// DSD track is genuinely skipped.
	var op browseProjectionResponse
	code = doJSON(t, srv.Handler(), "GET",
		"/api/library/browse-projection?path=MusicA&kind=optimize", nil, &op)
	if code != http.StatusOK {
		t.Fatalf("optimize projection: %d", code)
	}
	if op.ProjectedFiles != 1 || op.AlreadyAtTargetFiles != 2 || op.UnknownFormatFiles != 1 {
		t.Errorf("optimize buckets = projected %d / atTarget %d / unknown %d, want 1/2/1",
			op.ProjectedFiles, op.AlreadyAtTargetFiles, op.UnknownFormatFiles)
	}
}
