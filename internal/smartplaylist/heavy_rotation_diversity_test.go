package smartplaylist

import (
	"fmt"
	"testing"
)

// When the diversity caps would starve the mix below MinHeavyRotation (a pool
// dominated by a few artists/albums), buildHeavyRotation falls back to the
// UNCAPPED list so the family shows instead of hiding (the raw pool already met
// the floor upstream).
func TestBuildHeavyRotation_capStarvation_fallsBackUncapped(t *testing.T) {
	feats := map[string]TrackFeature{}
	var stats []PlayStat
	for i := 0; i < 12; i++ {
		p := fmt.Sprintf("dom/%d.flac", i)
		feats[p] = TrackFeature{Path: p, Title: fmt.Sprintf("T%d", i), Artist: "One", Album: "Alb"}
		stats = append(stats, PlayStat{Path: p, Plays: 5})
	}
	in := Inputs{HeavyRotation: stats, Features: feats}
	opts := Options{MaxItems: 50, MinHeavyRotation: 10, HeavyRotationPerArtistCap: 4, HeavyRotationPerAlbumCap: 4}
	pl, ok := buildHeavyRotation(in, opts)
	if !ok {
		t.Fatalf("expected the family to be generated via the uncapped fallback")
	}
	if len(pl.Items) != 12 {
		t.Fatalf("uncapped fallback: want all 12 items, got %d", len(pl.Items))
	}
}

// itemsFromPathsDiverse enforces per-artist / per-album diversity caps on the
// Heavy Rotation build so one heavily-played artist / album can't flood the
// mix (the "13 Beatles tracks in a row" case).

func TestItemsFromPathsDiverse_perArtistCap(t *testing.T) {
	feats := map[string]TrackFeature{
		"b1": {Path: "b1", Title: "T1", Artist: "The Beatles", Album: "A"},
		"b2": {Path: "b2", Title: "T2", Artist: "The Beatles", Album: "A"},
		"b3": {Path: "b3", Title: "T3", Artist: "the beatles", Album: "A"}, // case variant → same act
		"b4": {Path: "b4", Title: "T4", Artist: "The Beatles", Album: "B"},
		"x1": {Path: "x1", Title: "X1", Artist: "ABBA", Album: "Z"},
	}
	items := itemsFromPathsDiverse([]string{"b1", "b2", "b3", "b4", "x1"}, feats, 50, 2, 0)
	beatles := 0
	for _, it := range items {
		if diversityKey(it.Artist) == "the beatles" {
			beatles++
		}
	}
	if beatles != 2 {
		t.Fatalf("per-artist cap 2: want 2 Beatles, got %d (%+v)", beatles, items)
	}
	if len(items) != 3 { // 2 Beatles + 1 ABBA
		t.Fatalf("want 3 items, got %d (%+v)", len(items), items)
	}
}

func TestItemsFromPathsDiverse_perAlbumCap(t *testing.T) {
	feats := map[string]TrackFeature{
		"a1": {Path: "a1", Artist: "V", Album: "Album1"},
		"a2": {Path: "a2", Artist: "W", Album: "Album1"},
		"a3": {Path: "a3", Artist: "X", Album: "Album1"},
		"a4": {Path: "a4", Artist: "Y", Album: "Album2"},
	}
	items := itemsFromPathsDiverse([]string{"a1", "a2", "a3", "a4"}, feats, 50, 0, 2)
	if len(items) != 3 { // 2 from Album1 + 1 from Album2
		t.Fatalf("per-album cap 2: want 3, got %d (%+v)", len(items), items)
	}
}

func TestItemsFromPathsDiverse_zeroCapDisables(t *testing.T) {
	feats := map[string]TrackFeature{
		"b1": {Path: "b1", Artist: "One", Album: "A"},
		"b2": {Path: "b2", Artist: "One", Album: "A"},
		"b3": {Path: "b3", Artist: "One", Album: "A"},
	}
	items := itemsFromPathsDiverse([]string{"b1", "b2", "b3"}, feats, 50, 0, 0)
	if len(items) != 3 {
		t.Fatalf("caps disabled: want all 3, got %d", len(items))
	}
}

func TestItemsFromPathsDiverse_emptyKeyNotCapped(t *testing.T) {
	// Blank artist/album must NOT be collapsed by the cap.
	feats := map[string]TrackFeature{
		"e1": {Path: "e1", Artist: "", Album: ""},
		"e2": {Path: "e2", Artist: "", Album: ""},
		"e3": {Path: "e3", Artist: "", Album: ""},
	}
	items := itemsFromPathsDiverse([]string{"e1", "e2", "e3"}, feats, 50, 1, 1)
	if len(items) != 3 {
		t.Fatalf("empty keys must not be capped: want 3, got %d", len(items))
	}
}

func TestItemsFromPathsDiverse_preservesOrder(t *testing.T) {
	// Input (plays-desc) order is preserved for the kept items.
	feats := map[string]TrackFeature{
		"p1": {Path: "p1", Artist: "A", Album: "1"},
		"p2": {Path: "p2", Artist: "B", Album: "2"},
		"p3": {Path: "p3", Artist: "C", Album: "3"},
	}
	items := itemsFromPathsDiverse([]string{"p1", "p2", "p3"}, feats, 50, 4, 4)
	if len(items) != 3 || items[0].Path != "p1" || items[1].Path != "p2" || items[2].Path != "p3" {
		t.Fatalf("order not preserved: %+v", items)
	}
	// Positions are re-sequenced 0..n.
	for i, it := range items {
		if it.Position != i {
			t.Fatalf("position %d != index %d", it.Position, i)
		}
	}
}
