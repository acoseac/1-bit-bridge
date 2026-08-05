package manifest

import (
	"bytes"
	"context"
	"testing"
)

// --- ReplaceSmartPlaylistFamily (per-family regenerate) ---

func spSeedFamilies(t *testing.T, s *Store, slugs ...string) {
	t.Helper()
	rows := make([]StoredSmartPlaylist, 0, len(slugs))
	for i, slug := range slugs {
		rows = append(rows, StoredSmartPlaylist{
			Slug: slug, Kind: "listening", Title: "Title " + slug,
			Position: i, RefreshedAt: 1000, ItemsJSON: []byte(`[{"position":0,"path":"a/` + slug + `.flac"}]`),
		})
	}
	if err := s.ReplaceSmartPlaylists(context.Background(), rows); err != nil {
		t.Fatalf("ReplaceSmartPlaylists: %v", err)
	}
}

func spLoadBySlug(t *testing.T, s *Store) map[string]StoredSmartPlaylist {
	t.Helper()
	rows, err := s.LoadSmartPlaylists(context.Background())
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	out := make(map[string]StoredSmartPlaylist, len(rows))
	for _, r := range rows {
		out[r.Slug] = r
	}
	return out
}

func TestReplaceSmartPlaylistFamily_PreservesPositionAndSiblings(t *testing.T) {
	s := newSPStore(t)
	spSeedFamilies(t, s, "heavyRotation", "driveMix", "autoMix")
	before := spLoadBySlug(t, s)

	fresh := &StoredSmartPlaylist{
		Slug: "driveMix", Kind: "listening", Title: "Drive Mix", Subtitle: "Your road favorites",
		// Position deliberately wrong: the method must keep the CACHED slot,
		// not trust the fresh engine output's index.
		Position: 99, RefreshedAt: 2000, ItemsJSON: []byte(`[{"position":0,"path":"b/new.flac"}]`),
	}
	existed, err := s.ReplaceSmartPlaylistFamily(context.Background(), "driveMix", fresh)
	if err != nil {
		t.Fatalf("ReplaceSmartPlaylistFamily: %v", err)
	}
	if !existed {
		t.Fatalf("existed = false, want true (driveMix was cached)")
	}

	after := spLoadBySlug(t, s)
	if got := after["driveMix"]; got.Position != 1 || got.RefreshedAt != 2000 ||
		!bytes.Equal(got.ItemsJSON, fresh.ItemsJSON) {
		t.Fatalf("driveMix not replaced in place: pos=%d refreshed=%d items=%s",
			got.Position, got.RefreshedAt, got.ItemsJSON)
	}
	for _, sib := range []string{"heavyRotation", "autoMix"} {
		b, a := before[sib], after[sib]
		if a.Position != b.Position || a.RefreshedAt != b.RefreshedAt || !bytes.Equal(a.ItemsJSON, b.ItemsJSON) {
			t.Fatalf("sibling %s was touched: before=%+v after=%+v", sib, b, a)
		}
	}
}

func TestReplaceSmartPlaylistFamily_AppendsNewSlugAfterMax(t *testing.T) {
	s := newSPStore(t)
	spSeedFamilies(t, s, "heavyRotation", "driveMix")

	fresh := &StoredSmartPlaylist{Slug: "onRepeat", Kind: "listening", Title: "On Repeat",
		RefreshedAt: 2000, ItemsJSON: []byte(`[]`)}
	existed, err := s.ReplaceSmartPlaylistFamily(context.Background(), "onRepeat", fresh)
	if err != nil {
		t.Fatalf("ReplaceSmartPlaylistFamily: %v", err)
	}
	if existed {
		t.Fatalf("existed = true, want false (onRepeat was not cached)")
	}
	if got := spLoadBySlug(t, s)["onRepeat"]; got.Position != 2 {
		t.Fatalf("new family position = %d, want 2 (append after max)", got.Position)
	}
}

func TestReplaceSmartPlaylistFamily_FirstFamilyLandsAtZero(t *testing.T) {
	s := newSPStore(t)
	fresh := &StoredSmartPlaylist{Slug: "recentlyPlayed", Kind: "listening", Title: "Recently Played",
		RefreshedAt: 2000, ItemsJSON: []byte(`[]`)}
	if _, err := s.ReplaceSmartPlaylistFamily(context.Background(), "recentlyPlayed", fresh); err != nil {
		t.Fatalf("ReplaceSmartPlaylistFamily: %v", err)
	}
	if got := spLoadBySlug(t, s)["recentlyPlayed"]; got.Position != 0 {
		t.Fatalf("first family position = %d, want 0", got.Position)
	}
}

func TestReplaceSmartPlaylistFamily_NilRowDeletes(t *testing.T) {
	s := newSPStore(t)
	spSeedFamilies(t, s, "heavyRotation", "driveMix")

	existed, err := s.ReplaceSmartPlaylistFamily(context.Background(), "driveMix", nil)
	if err != nil {
		t.Fatalf("ReplaceSmartPlaylistFamily(nil): %v", err)
	}
	if !existed {
		t.Fatalf("existed = false, want true on first delete")
	}
	after := spLoadBySlug(t, s)
	if _, ok := after["driveMix"]; ok {
		t.Fatalf("driveMix still cached after nil-row replace")
	}
	if _, ok := after["heavyRotation"]; !ok {
		t.Fatalf("sibling heavyRotation was deleted")
	}

	existed, err = s.ReplaceSmartPlaylistFamily(context.Background(), "driveMix", nil)
	if err != nil {
		t.Fatalf("ReplaceSmartPlaylistFamily(nil, second): %v", err)
	}
	if existed {
		t.Fatalf("existed = true on second delete, want false")
	}
}

func TestReplaceSmartPlaylistFamily_Validation(t *testing.T) {
	s := newSPStore(t)
	if _, err := s.ReplaceSmartPlaylistFamily(context.Background(), "", nil); err == nil {
		t.Fatalf("empty slug accepted")
	}
	row := &StoredSmartPlaylist{Slug: "other", ItemsJSON: []byte(`[]`)}
	if _, err := s.ReplaceSmartPlaylistFamily(context.Background(), "driveMix", row); err == nil {
		t.Fatalf("slug/row mismatch accepted")
	}
}
