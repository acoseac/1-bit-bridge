package admin

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestArtistDetailSummarisesTheWholeDiscography: the artist page is
// where a bulk request actually belongs, so its numbers have to be the
// union of its albums — not a sample, and not a second definition of
// "this artist's tracks" that could drift from the albums on screen.
func TestArtistDetailSummarisesTheWholeDiscography(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)

	id := artistIDByName(t, srv, "Artist")
	w, body := playerGet(t, srv, "/api/player/artists/"+id)
	if w.Code != http.StatusOK {
		t.Fatalf("artist detail: status %d body %s", w.Code, w.Body.String())
	}

	var sum playerVariantSummaryDTO
	blob, _ := json.Marshal(body["variants"])
	if err := json.Unmarshal(blob, &sum); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	// Four tracks across three albums: two carry a CarPlay copy, three
	// could take one (the CD track is already at that target).
	if sum.Optimize.Covered != 2 || sum.Optimize.Eligible != 3 {
		t.Errorf("optimize = %+v, want covered 2 / eligible 3", sum.Optimize)
	}
	if sum.Optimize.Exempt != 1 {
		t.Errorf("optimize exempt = %d, want 1", sum.Optimize.Exempt)
	}
	if sum.SourceBytes != 4000 {
		t.Errorf("sourceBytes = %d, want 4000 (the sum of its albums)", sum.SourceBytes)
	}
	if sum.VariantBytes != 200 {
		t.Errorf("variantBytes = %d, want 200", sum.VariantBytes)
	}
}

// TestArtistAndAlbumSummariesAgree: both views run the same code over
// the same store, so an album's numbers must be exactly the artist's
// restricted to that album. They are two entry points to one answer,
// and this is what stops them becoming two answers.
func TestArtistAndAlbumSummariesAgree(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)

	var artistSum playerVariantSummaryDTO
	_, body := playerGet(t, srv, "/api/player/artists/"+artistIDByName(t, srv, "Artist"))
	blob, _ := json.Marshal(body["variants"])
	_ = json.Unmarshal(blob, &artistSum)

	totals := playerVariantCoverageDTO{}
	var bytes int64
	for _, title := range []string{"Covered", "Bare", "Redbook"} {
		var s playerVariantSummaryDTO
		b, _ := json.Marshal(albumDetailBody(t, srv, title)["variants"])
		_ = json.Unmarshal(b, &s)
		totals.Covered += s.Optimize.Covered
		totals.Eligible += s.Optimize.Eligible
		totals.Stale += s.Optimize.Stale
		bytes += s.SourceBytes
	}
	if totals.Covered != artistSum.Optimize.Covered ||
		totals.Eligible != artistSum.Optimize.Eligible ||
		totals.Stale != artistSum.Optimize.Stale {
		t.Errorf("album totals %+v disagree with the artist summary %+v",
			totals, artistSum.Optimize)
	}
	if bytes != artistSum.SourceBytes {
		t.Errorf("album sourceBytes sum %d != artist %d", bytes, artistSum.SourceBytes)
	}
}
