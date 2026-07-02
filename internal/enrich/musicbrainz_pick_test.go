package enrich

import "testing"

// TestPickBestRelease_LinearScanMatchesSortSemantics guards the F22
// refactor: pickBestRelease now selects the top-scoring candidate via an
// O(n) linear max-scan instead of sort.SliceStable(...); return out[0].
// The strict `>` in the scan must preserve the "first of equal top score
// wins" tie-break the stable sort produced, and all the pre-scan filters
// (>=80 score, album substring, artist substring) + exact-match bonuses
// must behave exactly as before.
func TestPickBestRelease_LinearScanMatchesSortSemantics(t *testing.T) {
	const album, artist = "Kind of Blue", "Miles Davis"
	credit := []artistCredit{{Name: "Miles Davis"}}

	t.Run("highest score wins", func(t *testing.T) {
		cands := []releaseCandidate{
			{ID: "a", Score: 85, Title: album, ArtistCredit: credit},
			{ID: "b", Score: 95, Title: album, ArtistCredit: credit},
			{ID: "c", Score: 88, Title: album, ArtistCredit: credit},
		}
		if got := pickBestRelease(cands, album, artist); got == nil || got.ID != "b" {
			t.Fatalf("got %v, want candidate b (highest score)", got)
		}
	})

	t.Run("tie-break keeps first of equal top score", func(t *testing.T) {
		// Both reach 90+10(title)+10(artist) = 110; the first must win.
		cands := []releaseCandidate{
			{ID: "first", Score: 90, Title: album, ArtistCredit: credit},
			{ID: "second", Score: 90, Title: album, ArtistCredit: credit},
		}
		if got := pickBestRelease(cands, album, artist); got == nil || got.ID != "first" {
			t.Fatalf("got %v, want candidate first (stable tie-break)", got)
		}
	})

	t.Run("exact-match bonus outranks a higher raw score", func(t *testing.T) {
		// fuzzy: raw 100, substring-only matches, no bonuses -> 100.
		// exact: raw 92 + exact title(+10) + exact artist(+10) -> 112 wins.
		cands := []releaseCandidate{
			{ID: "fuzzy", Score: 100, Title: "Kind of Blue (Deluxe)", ArtistCredit: []artistCredit{{Name: "Miles Davis Quintet"}}},
			{ID: "exact", Score: 92, Title: album, ArtistCredit: credit},
		}
		if got := pickBestRelease(cands, album, artist); got == nil || got.ID != "exact" {
			t.Fatalf("got %v, want candidate exact (112 > 100)", got)
		}
	})

	t.Run("filters sub-80 and non-matching; nil when none qualify", func(t *testing.T) {
		cands := []releaseCandidate{
			{ID: "lowscore", Score: 70, Title: album, ArtistCredit: credit},
			{ID: "wrongalbum", Score: 99, Title: "Bitches Brew", ArtistCredit: credit},
			{ID: "wrongartist", Score: 99, Title: album, ArtistCredit: []artistCredit{{Name: "John Coltrane"}}},
		}
		if got := pickBestRelease(cands, album, artist); got != nil {
			t.Fatalf("got %v, want nil (no candidate qualifies)", got)
		}
		if got := pickBestRelease(nil, album, artist); got != nil {
			t.Fatalf("got %v, want nil (empty candidates)", got)
		}
	})
}
