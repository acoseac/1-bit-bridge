package admin

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestTheConsoleShowsTheChristmasFamilyAsServedToEveryFormat: the regenerator
// stores the Christmas family as the material each request's view is picked
// from, up to 100 tracks of each format (round 5 of backlog B489), so the
// console counts and lists it as the pick of a client that declares every
// format, 100 tracks at most, where every other family is its stored items.
func TestTheConsoleShowsTheChristmasFamilyAsServedToEveryFormat(t *testing.T) {
	var items []manifest.SmartPlaylistItem
	for _, f := range []struct {
		ext string
		n   int
	}{{".flac", 40}, {".wv", 100}, {".ape", 100}} {
		for i := 0; i < f.n; i++ {
			items = append(items, manifest.SmartPlaylistItem{Position: len(items),
				Path: fmt.Sprintf("Lib/%03d%s", i, f.ext), Artist: fmt.Sprintf("Artist %d", i%5), Album: fmt.Sprintf("Album %d", i%4)})
		}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		kind string
		want int
	}{
		{"christmas", 100},
		{"heavyRotation", 240},
	} {
		row := manifest.StoredSmartPlaylist{Slug: c.kind, Kind: c.kind, ItemsJSON: raw}
		if got := smartPlaylistItemCount(row); got != c.want {
			t.Errorf("%s: the console counts %d items, want %d", c.kind, got, c.want)
		}
		views := smartMixTracksForView(row)
		if len(views) != c.want {
			t.Fatalf("%s: the console lists %d items, want %d", c.kind, len(views), c.want)
		}
		// The 40 FLACs first, then the two gated formats in turn: the 41st
		// a WavPack file, the 42nd a Monkey's Audio one.
		if c.kind == "christmas" && (views[0].Path != "Lib/000.flac" || views[40].Path != "Lib/000.wv" || views[41].Path != "Lib/000.ape") {
			t.Errorf("christmas: the console lists %s, %s and %s at 1, 41 and 42; want the FLACs first and the gated formats in turn after them",
				views[0].Path, views[40].Path, views[41].Path)
		}
	}
}
