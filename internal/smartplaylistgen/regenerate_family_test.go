package smartplaylistgen

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedRegenFixture seeds tracks + analysis + 3 daily CarPlay sessions and
// runs a full Regenerate at `now`, returning the resulting cache rows keyed
// by slug. Mirrors TestRegenerate_PipelineEndToEnd's recipe.
func seedRegenFixture(t *testing.T, s *manifest.Store, now time.Time) map[string]manifest.StoredSmartPlaylist {
	t.Helper()
	ctx := context.Background()
	for _, p := range []string{"/a.flac", "/b.flac", "/c.flac"} {
		track(t, s, p, "Jazz")
	}
	analysis(t, s, "/a.flac", 0, "major", 120)
	analysis(t, s, "/b.flac", 7, "major", 122)
	analysis(t, s, "/c.flac", 9, "minor", 121)
	seedDailyPlays(t, s, now, "CarPlay", []int{3, 2, 1}, "/a.flac", "/b.flac", "/c.flac")

	if n, err := Regenerate(ctx, s, regenTestOptions(now.UnixNano(), true)); err != nil || n == 0 {
		t.Fatalf("seed Regenerate: n=%d err=%v", n, err)
	}
	rows, err := s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	out := make(map[string]manifest.StoredSmartPlaylist, len(rows))
	for _, r := range rows {
		out[r.Slug] = r
	}
	return out
}

func TestRegenerateFamily_TouchesOnlyRequestedSlug(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	before := seedRegenFixture(t, s, now)
	if _, ok := before["heavy-rotation"]; !ok {
		t.Fatalf("fixture did not generate heavy-rotation; slugs=%v", keysOf(before))
	}

	// Re-run ONE family a minute later: its row refreshes, siblings stay
	// byte-identical at the original RefreshedAt.
	later := now.Add(time.Minute)
	generated, existed, n, err := RegenerateFamily(ctx, s, regenTestOptions(later.UnixNano(), true), "heavy-rotation")
	if err != nil {
		t.Fatalf("RegenerateFamily: %v", err)
	}
	if !generated || !existed || n == 0 {
		t.Fatalf("generated=%v existed=%v n=%d, want true/true/>0", generated, existed, n)
	}

	rows, err := s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	if len(rows) != len(before) {
		t.Fatalf("family count changed: %d -> %d", len(before), len(rows))
	}
	for _, r := range rows {
		b := before[r.Slug]
		if r.Slug == "heavy-rotation" {
			if r.RefreshedAt != later.UnixNano() {
				t.Errorf("heavy-rotation RefreshedAt = %d, want %d", r.RefreshedAt, later.UnixNano())
			}
			if r.Position != b.Position {
				t.Errorf("heavy-rotation position changed: %d -> %d", b.Position, r.Position)
			}
			continue
		}
		if r.RefreshedAt != b.RefreshedAt || !bytes.Equal(r.ItemsJSON, b.ItemsJSON) || r.Position != b.Position {
			t.Errorf("sibling %s was touched: before{pos=%d ref=%d} after{pos=%d ref=%d}",
				r.Slug, b.Position, b.RefreshedAt, r.Position, r.RefreshedAt)
		}
	}
}

func TestRegenerateFamily_RemovesEmptiedFamily(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	before := seedRegenFixture(t, s, now)
	if _, ok := before["heavy-rotation"]; !ok {
		t.Fatalf("fixture did not generate heavy-rotation; slugs=%v", keysOf(before))
	}

	// 100 days later every play is outside the heavy-rotation window, so the
	// fresh run no longer populates the family — its cached row must go.
	later := now.Add(100 * 24 * time.Hour)
	generated, existed, n, err := RegenerateFamily(ctx, s, regenTestOptions(later.UnixNano(), true), "heavy-rotation")
	if err != nil {
		t.Fatalf("RegenerateFamily: %v", err)
	}
	if generated || !existed || n != 0 {
		t.Fatalf("generated=%v existed=%v n=%d, want false/true/0", generated, existed, n)
	}
	rows, err := s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	for _, r := range rows {
		if r.Slug == "heavy-rotation" {
			t.Fatalf("emptied heavy-rotation still cached")
		}
	}
	if len(rows) != len(before)-1 {
		t.Fatalf("expected exactly one row removed: %d -> %d", len(before), len(rows))
	}
}

func TestRegenerateFamily_UnknownSlugIsNoop(t *testing.T) {
	s := openGenStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	before := seedRegenFixture(t, s, now)

	generated, existed, n, err := RegenerateFamily(ctx, s, regenTestOptions(now.UnixNano(), true), "noSuchFamily")
	if err != nil {
		t.Fatalf("RegenerateFamily: %v", err)
	}
	if generated || existed || n != 0 {
		t.Fatalf("generated=%v existed=%v n=%d, want false/false/0", generated, existed, n)
	}
	rows, err := s.LoadSmartPlaylists(ctx)
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	if len(rows) != len(before) {
		t.Fatalf("cache size changed on unknown slug: %d -> %d", len(before), len(rows))
	}
}
