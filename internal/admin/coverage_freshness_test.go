package admin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// coverageReadout is every CarPlay coverage number the console draws for
// one two-track album, "Hi/Covered", read through the handlers that draw
// them.
type coverageReadout struct {
	rootTile      [3]int64 // the root listing's "Hi" tile: covered, eligible, bytes
	folderTile    [3]int64 // the "Hi" listing's "Hi/Covered" tile
	subtree       [3]int   // the "Hi/Covered" panel header: covered, eligible, stale
	trackMarks    [2]bool  // 01 and 02's CarPlay marks
	grid, detail  [3]int   // the album grid's and the album detail's covered, eligible, stale
	detailBytes   int64    // the album detail's variant bytes
	alreadyCover  int      // the projection's already-covered files
	projectedWork int      // the projection's projected files
}

func readCoverage(t *testing.T, srv *Server) coverageReadout {
	t.Helper()
	var r coverageReadout
	h := srv.Handler()
	var root, hi, album struct {
		Folders []browseFolderRow `json:"folders"`
		Tracks  []browseTrackRow  `json:"tracks"`
		browseResponse
	}
	if code := doJSON(t, h, "GET", "/api/library/browse?path=", nil, &root); code != http.StatusOK {
		t.Fatalf("browse root: %d", code)
	}
	r.rootTile = folderTile(t, root.Folders, "Hi")
	if code := doJSON(t, h, "GET", "/api/library/browse?path=Hi", nil, &hi); code != http.StatusOK {
		t.Fatalf("browse Hi: %d", code)
	}
	r.folderTile = folderTile(t, hi.Folders, "Hi/Covered")
	if code := doJSON(t, h, "GET", "/api/library/browse?path=Hi/Covered", nil, &album); code != http.StatusOK {
		t.Fatalf("browse Hi/Covered: %d", code)
	}
	eligible := -1
	if album.SubtreeOptimizeEligible != nil {
		eligible = *album.SubtreeOptimizeEligible
	}
	r.subtree = [3]int{album.SubtreeOptimized, eligible, album.SubtreeOptimizedStale}
	for _, tr := range album.Tracks {
		switch tr.Path {
		case "Hi/Covered/01.flac":
			r.trackMarks[0] = tr.IsOptimized
		case "Hi/Covered/02.flac":
			r.trackMarks[1] = tr.IsOptimized
		}
	}

	srv.InvalidateAlbumCoverage() // the grid's snapshot, which a sweep's write does not nudge either
	c := coverageOf(t, srv, "Covered")
	r.grid = [3]int{c.Covered, c.Eligible, c.Stale}
	var sum playerVariantSummaryDTO
	blob, _ := json.Marshal(albumDetailBody(t, srv, "Covered")["variants"])
	if err := json.Unmarshal(blob, &sum); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	r.detail = [3]int{sum.Optimize.Covered, sum.Optimize.Eligible, sum.Optimize.Stale}
	r.detailBytes = sum.VariantBytes

	var proj browseProjectionResponse
	if code := doJSON(t, h, "GET", "/api/library/browse-projection?path=Hi/Covered&kind=optimize", nil, &proj); code != http.StatusOK {
		t.Fatalf("projection: %d", code)
	}
	r.alreadyCover, r.projectedWork = proj.AlreadyCoveredFiles, proj.ProjectedFiles
	return r
}

func folderTile(t *testing.T, rows []browseFolderRow, path string) [3]int64 {
	t.Helper()
	for _, f := range rows {
		if f.Path != path {
			continue
		}
		eligible := int64(-1)
		if f.OptimizeEligibleCount != nil {
			eligible = int64(*f.OptimizeEligibleCount)
		}
		return [3]int64{int64(f.OptimizedCount), eligible, f.OptimizedSizeBytes}
	}
	t.Fatalf("no folder %s in %+v", path, rows)
	return [3]int64{}
}

// wireOptimizeProjection gives the projection endpoint what cmd/bridge
// wires for the CarPlay kind: the real eligibility gate and target rule,
// the real size projection, and a disk with room.
func wireOptimizeProjection(srv *Server) {
	srv.deps.ProjectedSize = func(sourceSize int64, sourceRate, sourceBits, targetRate, targetBits int) int64 {
		return transcode.ProjectedSize(sourceSize, sourceRate, sourceBits, targetRate, targetBits,
			transcode.DefaultCompressionFactor(targetBits))
	}
	srv.deps.AvailableDiskSpace = func(string) (int64, error) { return 1 << 40, nil }
	srv.deps.OptimizeEligible = transcode.OptimizeEligible
	srv.deps.TargetRateForOptimize = transcode.TargetRateForOptimize
	cfg := config.Clone(srv.deps.CfgHolder.Load())
	cfg.Upscale.Enabled = true
	srv.deps.CfgHolder.Store(cfg)
}

// TestTheCoverageBarsDropForAStaleRenditionUntilItIsRenderedAgain is
// backlog B100's other half: every coverage bar the console draws, and
// the projection that previews a Generate, counts a track only while one
// of its renditions of the kind is FRESH, as a batch now does. An album's
// two CarPlay copies read 2/2; a retag of one track, read by a scan, moves
// its row's stamp, and the bars drop to 1/2 with the copy counted stale
// (its bytes still counted: the disk is real) until a render stamped from
// the new row lands, when they read 2/2 again.
//
// On main the bars read 2/2 throughout: they counted ANY rendition of the
// kind, so an album whose copies answer 410 looked ready for CarPlay.
func TestTheCoverageBarsDropForAStaleRenditionUntilItIsRenderedAgain(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	wireOptimizeProjection(srv)
	seedTracks(t, st,
		fixtureTrack("Hi/Covered/01.flac", "a", "Covered", fixtureHiRate, fixtureHiBits),
		fixtureTrack("Hi/Covered/02.flac", "b", "Covered", fixtureHiRate, fixtureHiBits),
	)
	for _, p := range []string{"Hi", "Hi/Covered"} {
		if err := st.UpsertFolder(t.Context(), &manifest.Folder{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	addOptimizedVariant(t, st, "Hi/Covered/01.flac")
	addOptimizedVariant(t, st, "Hi/Covered/02.flac")

	full := coverageReadout{
		rootTile: [3]int64{2, 2, 200}, folderTile: [3]int64{2, 2, 200}, subtree: [3]int{2, 2, 0},
		trackMarks: [2]bool{true, true}, grid: [3]int{2, 2, 0}, detail: [3]int{2, 2, 0}, detailBytes: 200,
		alreadyCover: 2, projectedWork: 0,
	}
	if got := readCoverage(t, srv); got != full {
		t.Fatalf("both copies fresh: the console reads %+v, want %+v", got, full)
	}

	// A retag of 01, which a scan reads: its row's version moves past the
	// one its copy records.
	retagged := fixtureTrack("Hi/Covered/01.flac", "a", "Covered", fixtureHiRate, fixtureHiBits)
	retagged.ModTime, retagged.Size = fixtureMTime().Add(time.Minute), fixtureSize+8
	seedTracks(t, st, retagged)
	stale := coverageReadout{
		rootTile: [3]int64{1, 2, 200}, folderTile: [3]int64{1, 2, 200}, subtree: [3]int{1, 2, 1},
		trackMarks: [2]bool{false, true}, grid: [3]int{1, 2, 1}, detail: [3]int{1, 2, 1}, detailBytes: 200,
		alreadyCover: 1, projectedWork: 1,
	}
	if got := readCoverage(t, srv); got != stale {
		t.Errorf("after a retag and a scan: the console reads %+v, want %+v: the stale copy answers 410, so "+
			"the bars must not count it, and a Generate renders it", got, stale)
	}

	// The render a batch makes, committed as Pool.processJob commits it:
	// the family's current id, stamped from the row.
	if err := st.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: "Hi/Covered/01.flac", VariantID: "optimized-v2-48000-16", SidecarPath: "Hi/Covered/01.flac.x",
		Format: "FLAC", SampleRate: 48000, BitsPerSample: 16, SizeBytes: 100,
		SourceMTimeNS: retagged.ModTime.UnixNano(), SourceSize: retagged.Size,
	}); err != nil {
		t.Fatal(err)
	}
	if got := readCoverage(t, srv); got != full {
		t.Errorf("rendered again: the console reads %+v, want %+v", got, full)
	}
}
