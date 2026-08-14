package smartplaylist

import (
	"fmt"
	"testing"
)

// Builder-level tests for the Favorites family (the iOS favorites backup as
// a smart mix — F4/B2). Pure Inputs literals, no SQLite; the bridge-local
// pool restriction (foreign favorites never enter) is enforced by the
// manifest query and tested in internal/manifest.

func favOpts() Options {
	return Options{
		AnalysisEnabled:   false,
		MaxItems:          50,
		MinFavorites:      5,
		MaxFavoritesItems: 100,
	}
}

// favFeatures builds n favorites; the first `keyed` carry a Camelot-mappable
// key so the harmonic arm has material.
func favFeatures(n, keyed int) []TrackFeature {
	out := make([]TrackFeature, 0, n)
	for i := 0; i < n; i++ {
		f := TrackFeature{
			Path:   fmt.Sprintf("/fav%02d.flac", i),
			Title:  fmt.Sprintf("T%02d", i),
			Artist: "A",
		}
		if i < keyed {
			root := i % 12
			f.KeyRoot = &root
			f.KeyMode = "major"
			bpm := 100 + i
			f.BPM = &bpm
		}
		out = append(out, f)
	}
	return out
}

func TestBuildFavorites_BelowFloorNotPopulated(t *testing.T) {
	in := Inputs{Favorites: favFeatures(4, 0), WeekSeed: 1}
	if _, ok := buildFavorites(in, favOpts()); ok {
		t.Fatal("4 favorites under a MinFavorites of 5 must not populate")
	}
}

func TestBuildFavorites_SeededArm_WeeklyDeterministic(t *testing.T) {
	pool := favFeatures(20, 0)
	opts := favOpts() // AnalysisEnabled false → seeded arm

	a1, ok := buildFavorites(Inputs{Favorites: pool, WeekSeed: 42}, opts)
	if !ok {
		t.Fatal("populated pool must build")
	}
	a2, _ := buildFavorites(Inputs{Favorites: pool, WeekSeed: 42}, opts)
	if fmt.Sprint(a1.Items) != fmt.Sprint(a2.Items) {
		t.Error("same WeekSeed must yield an identical order (deterministic weekly shuffle)")
	}
	b, _ := buildFavorites(Inputs{Favorites: pool, WeekSeed: 43}, opts)
	if fmt.Sprint(a1.Items) == fmt.Sprint(b.Items) {
		t.Error("a different WeekSeed should rotate the order")
	}
	if a1.Slug != "favorites" || a1.Kind != KindFavorites {
		t.Errorf("identity mismatch: %q / %q", a1.Slug, a1.Kind)
	}
	// Every heart survives (cap is far above the pool).
	if len(a1.Items) != len(pool) {
		t.Errorf("want all %d hearts, got %d", len(pool), len(a1.Items))
	}
}

func TestBuildFavorites_CapApplied(t *testing.T) {
	opts := favOpts()
	opts.MaxFavoritesItems = 10
	got, ok := buildFavorites(Inputs{Favorites: favFeatures(20, 0), WeekSeed: 7}, opts)
	if !ok {
		t.Fatal("populated pool must build")
	}
	if len(got.Items) != 10 {
		t.Errorf("cap of 10 not applied: got %d items", len(got.Items))
	}
	// Positions are dense from 0 (the wire contract every family shares).
	for i, it := range got.Items {
		if it.Position != i {
			t.Fatalf("positions must be dense: item %d has Position %d", i, it.Position)
		}
	}
}

// The harmonic arm engages with analysis on + enough keyed hearts — and an
// un-keyed favorite is APPENDED, never dropped (a heart is explicit
// curation; losing it because the track wasn't analyzable would read as a
// bug to the user).
func TestBuildFavorites_HarmonicArm_KeepsUnkeyedHearts(t *testing.T) {
	opts := favOpts()
	opts.AnalysisEnabled = true
	pool := favFeatures(9, 6) // 6 keyed + 3 un-keyed
	got, ok := buildFavorites(Inputs{Favorites: pool, WeekSeed: 5}, opts)
	if !ok {
		t.Fatal("populated pool must build")
	}
	if len(got.Items) != len(pool) {
		t.Fatalf("un-keyed hearts must append after the harmonic run: want %d, got %d",
			len(pool), len(got.Items))
	}
	// The harmonic run leads, so the first item is a keyed track.
	first := got.Items[0].Path
	keyed := map[string]bool{}
	for i, f := range pool {
		if i < 6 {
			keyed[f.Path] = true
		}
	}
	if !keyed[first] {
		t.Errorf("harmonic arm must lead with a keyed track, got %q", first)
	}
	// No duplicates across the run + the appended tail.
	seen := map[string]bool{}
	for _, it := range got.Items {
		if seen[it.Path] {
			t.Fatalf("duplicate path %q in the mix", it.Path)
		}
		seen[it.Path] = true
	}
}

// Too few keyed hearts → the harmonic arm stands down and the seeded
// shuffle carries the whole pool (never an empty/short mix).
func TestBuildFavorites_FallsBackWhenTooFewKeyed(t *testing.T) {
	opts := favOpts()
	opts.AnalysisEnabled = true
	pool := favFeatures(8, 2) // only 2 keyed — under the harmonic floor of 5
	got, ok := buildFavorites(Inputs{Favorites: pool, WeekSeed: 5}, opts)
	if !ok {
		t.Fatal("populated pool must build")
	}
	if len(got.Items) != len(pool) {
		t.Errorf("fallback arm must carry the whole pool: want %d, got %d",
			len(pool), len(got.Items))
	}
}

// Generate places favorites in the homepage order (right after Heavy
// Rotation) when populated — the explicit-signal family outranks the
// play-derived shelves.
func TestGenerate_FavoritesPlacedAfterHeavyRotation(t *testing.T) {
	paths := make([]string, 12)
	for i := range paths {
		paths[i] = fmt.Sprintf("/h%02d.flac", i)
	}
	in := Inputs{
		HeavyRotation: makePlayStats(paths),
		Features:      featurePool(paths...),
		Favorites:     favFeatures(6, 0),
		WeekSeed:      1,
	}
	opts := favOpts()
	opts.MinHeavyRotation = 10
	gen := Generate(in, opts)
	if len(gen) < 2 {
		t.Fatalf("want heavy rotation + favorites, got %d families", len(gen))
	}
	if gen[0].Kind != KindHeavyRotation || gen[1].Kind != KindFavorites {
		t.Errorf("homepage order mismatch: %q then %q", gen[0].Kind, gen[1].Kind)
	}
}
