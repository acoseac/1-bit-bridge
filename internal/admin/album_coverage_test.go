package admin

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedCoverageLibrary stages three albums with deliberately different
// variant states: one fully CarPlay-covered, one untouched, and one
// CD-quality album that can never take a CarPlay copy at all.
func seedCoverageLibrary(t *testing.T, st *manifest.Store) {
	t.Helper()
	hi, hiBits, no := 96000.0, 24, false
	cd, cdBits := 44100.0, 16
	mk := func(path, title, album string, rate float64, bits int) *manifest.Track {
		return &manifest.Track{
			Path: path, Title: title, Album: album, AlbumArtist: "Artist", Artist: "Artist",
			Codec: "FLAC", Size: 1000, ModTime: time.Unix(7, 0),
			SampleRate: &rate, BitsPerSample: &bits, IsDSD: &no,
		}
	}
	for _, tr := range []*manifest.Track{
		mk("Hi/Covered/01.flac", "a", "Covered", hi, hiBits),
		mk("Hi/Covered/02.flac", "b", "Covered", hi, hiBits),
		mk("Hi/Bare/01.flac", "c", "Bare", hi, hiBits),
		mk("Cd/Redbook/01.flac", "d", "Redbook", cd, cdBits),
	} {
		if err := st.UpsertTrack(t.Context(), tr); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"Hi/Covered/01.flac", "Hi/Covered/02.flac"} {
		if err := st.UpsertVariant(t.Context(), manifest.VariantRow{
			SourcePath: p, VariantID: "optimized-v2-48000-16", SidecarPath: p + ".x",
			Format: "FLAC", SampleRate: 48000, BitsPerSample: 16, SizeBytes: 100,
			SourceMTimeNS: time.Unix(7, 0).UnixNano(), SourceSize: 1000,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func albumsPage(t *testing.T, srv *Server, query string) (int, []map[string]any) {
	t.Helper()
	w, body := playerGet(t, srv, "/api/player/albums?limit=50&"+query)
	if w.Code != http.StatusOK {
		t.Fatalf("albums?%s: status %d body %s", query, w.Code, w.Body.String())
	}
	total := 0
	if f, ok := body["total"].(float64); ok {
		total = int(f)
	}
	raw, _ := body["albums"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, a := range raw {
		m, _ := a.(map[string]any)
		out = append(out, m)
	}
	return total, out
}

func titlesOf(albums []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, a := range albums {
		if s, ok := a["title"].(string); ok {
			out[s] = true
		}
	}
	return out
}

// TestAlbumGridCarriesCoveragePerAlbum: every tile gets its own numbers,
// against an eligible denominator. The CD album's optimize denominator
// is zero — it is already at the CarPlay target — which is the case a
// track-count denominator would have rendered as "0 of 1 missing".
func TestAlbumGridCarriesCoveragePerAlbum(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)

	_, albums := albumsPage(t, srv, "")
	byTitle := map[string]albumCoverage{}
	for _, a := range albums {
		var cov albumCoverage
		blob, _ := json.Marshal(a["variants"])
		_ = json.Unmarshal(blob, &cov)
		byTitle[a["title"].(string)] = cov
	}
	if got := byTitle["Covered"].Optimize; got.Covered != 2 || got.Eligible != 2 {
		t.Errorf("Covered optimize = %+v, want 2/2", got)
	}
	if got := byTitle["Bare"].Optimize; got.Covered != 0 || got.Eligible != 1 {
		t.Errorf("Bare optimize = %+v, want 0/1", got)
	}
	if got := byTitle["Redbook"].Optimize; got.Eligible != 0 || got.Exempt != 1 {
		t.Errorf("Redbook optimize = %+v, want eligible 0 / exempt 1", got)
	}
}

// TestAlbumGridNeedsFilterIsWholeLibrary is the reason the coverage
// snapshot exists rather than a per-page query. A filter applied to the
// PAGE would draw page 1 of the filtered list from page 1 of the
// unfiltered one, and report a total for the wrong set — so this checks
// the TOTAL as well as the contents.
func TestAlbumGridNeedsFilterIsWholeLibrary(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)

	total, albums := albumsPage(t, srv, "needs=optimize")
	got := titlesOf(albums)
	if total != 1 || !got["Bare"] {
		t.Fatalf("needs=optimize → total %d %v, want just Bare", total, got)
	}
	if got["Redbook"] {
		t.Error("an album already at the CarPlay target was listed as needing one")
	}
	if got["Covered"] {
		t.Error("a fully covered album was listed as needing more")
	}

	// Nothing has an upscale variant, and three of the four tracks are
	// upscale-eligible — so every album carrying one of those is listed.
	total, albums = albumsPage(t, srv, "needs=upscale")
	if total != 3 {
		t.Errorf("needs=upscale → total %d, want 3: %v", total, titlesOf(albums))
	}
}

// TestAlbumGridStaleFilter: an out-of-date copy is the one state a full
// bar hides, so it gets its own filter.
func TestAlbumGridStaleFilter(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	seedCoverageLibrary(t, st)

	if total, _ := albumsPage(t, srv, "needs=stale"); total != 0 {
		t.Fatalf("needs=stale on a fresh library → %d, want 0", total)
	}
	// Re-stamp one sidecar against a source that has moved on.
	if err := st.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: "Hi/Covered/01.flac", VariantID: "optimized-v2-48000-16",
		SidecarPath: "x", Format: "FLAC", SampleRate: 48000, BitsPerSample: 16,
		SizeBytes: 100, SourceMTimeNS: 1, SourceSize: 99,
	}); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAlbumCoverage()

	total, albums := albumsPage(t, srv, "needs=stale")
	if total != 1 || !titlesOf(albums)["Covered"] {
		t.Fatalf("needs=stale → total %d %v, want just Covered", total, titlesOf(albums))
	}
	// It is still COVERED: the batch walks skip a track that has a
	// variant of the kind, so promising otherwise would offer a
	// Generate that enqueues nothing.
	var cov albumCoverage
	blob, _ := json.Marshal(albums[0]["variants"])
	_ = json.Unmarshal(blob, &cov)
	if cov.Optimize.Covered != 2 || cov.Optimize.Stale != 1 {
		t.Errorf("Covered optimize = %+v, want covered 2 / stale 1", cov.Optimize)
	}
}

// TestAlbumGridRejectsAnUnknownNeedsValue: an unrecognised filter is a
// 400, never a silent fall-through to "everything". A typo that widened
// the view would read as the filter being broken.
func TestAlbumGridRejectsAnUnknownNeedsValue(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)
	w, _ := playerGet(t, srv, "/api/player/albums?needs=bogus")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", w.Code, w.Body.String())
	}
}

// TestAlbumCoverageSnapshotKeysOnTheUpscaleTarget pins why eligibility
// is NOT baked into the library catalog: it depends on a runtime
// setting the catalog knows nothing about. Moving the target must move
// the denominators, not leave them until the next scan.
func TestAlbumCoverageSnapshotKeysOnTheUpscaleTarget(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	seedCoverageLibrary(t, st)

	// A 192/24 target leaves the 96/24 tracks upscale-eligible.
	if err := st.SetUpscaleTarget(t.Context(), 192000, 24); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAlbumCoverage()
	if total, albums := albumsPage(t, srv, "needs=upscale"); total != 3 {
		t.Fatalf("at 192/24 → total %d, want 3: %v", total, titlesOf(albums))
	}

	// Drop the target to 48/16 and the hi-res tracks are above it —
	// never downsampled, so no longer eligible. Only the CD track is.
	if err := st.SetUpscaleTarget(t.Context(), 48000, 16); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAlbumCoverage()
	total, albums := albumsPage(t, srv, "needs=upscale")
	if total != 1 || !titlesOf(albums)["Redbook"] {
		t.Fatalf("at 48/16 → total %d %v, want just Redbook", total, titlesOf(albums))
	}
}
