package manifest

import (
	"context"
	"testing"
)

func TestPlaylistCoverRoundTrip(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()

	// Missing → ok=false.
	if _, ok, err := s.GetPlaylistCover(ctx, CoverScopeSmartMix, "heavy-rotation"); err != nil || ok {
		t.Fatalf("expected miss; ok=%v err=%v", ok, err)
	}

	if err := s.SetPlaylistCover(ctx, PlaylistCover{
		Scope: CoverScopeSmartMix, Key: "heavy-rotation", ImageHash: "abc123", UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, ok, err := s.GetPlaylistCover(ctx, CoverScopeSmartMix, "heavy-rotation")
	if err != nil || !ok {
		t.Fatalf("get after set: ok=%v err=%v", ok, err)
	}
	if got.ImageHash != "abc123" || got.Ext != "jpg" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Upsert replaces hash + bumps updated_at; ext defaults to jpg.
	if err := s.SetPlaylistCover(ctx, PlaylistCover{
		Scope: CoverScopeSmartMix, Key: "heavy-rotation", ImageHash: "def456", UpdatedAt: 200,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, _ = s.GetPlaylistCover(ctx, CoverScopeSmartMix, "heavy-rotation")
	if got.ImageHash != "def456" || got.UpdatedAt != 200 {
		t.Errorf("upsert did not replace: %+v", got)
	}

	// Empty required fields rejected.
	if err := s.SetPlaylistCover(ctx, PlaylistCover{Scope: "", Key: "x", ImageHash: "h"}); err == nil {
		t.Error("expected error for empty scope")
	}
}

func TestPlaylistCoversByScope_isolatedByScope(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	_ = s.SetPlaylistCover(ctx, PlaylistCover{Scope: CoverScopeSmartMix, Key: "auto-mix", ImageHash: "m1", UpdatedAt: 1})
	_ = s.SetPlaylistCover(ctx, PlaylistCover{Scope: CoverScopeSmartMix, Key: "daily-mix", ImageHash: "m2", UpdatedAt: 1})
	_ = s.SetPlaylistCover(ctx, PlaylistCover{Scope: CoverScopePlaylist, Key: "uuid-1", ImageHash: "p1", UpdatedAt: 1})

	mix, err := s.PlaylistCoversByScope(ctx, CoverScopeSmartMix)
	if err != nil {
		t.Fatalf("by scope: %v", err)
	}
	if len(mix) != 2 || mix["auto-mix"].ImageHash != "m1" || mix["daily-mix"].ImageHash != "m2" {
		t.Errorf("smartmix scope wrong: %+v", mix)
	}
	if _, leaked := mix["uuid-1"]; leaked {
		t.Error("playlist-scope cover leaked into smartmix scope")
	}
	pl, _ := s.PlaylistCoversByScope(ctx, CoverScopePlaylist)
	if len(pl) != 1 || pl["uuid-1"].ImageHash != "p1" {
		t.Errorf("playlist scope wrong: %+v", pl)
	}
}

func TestDeletePlaylistCover(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	_ = s.SetPlaylistCover(ctx, PlaylistCover{Scope: CoverScopePlaylist, Key: "uuid-1", ImageHash: "h", Ext: "jpg", UpdatedAt: 1})

	hash, ext, ok, err := s.DeletePlaylistCover(ctx, CoverScopePlaylist, "uuid-1")
	if err != nil || !ok || hash != "h" || ext != "jpg" {
		t.Fatalf("delete: hash=%q ext=%q ok=%v err=%v", hash, ext, ok, err)
	}
	if _, ok, _ := s.GetPlaylistCover(ctx, CoverScopePlaylist, "uuid-1"); ok {
		t.Error("cover still present after delete")
	}
	// Deleting a missing row is not an error, ok=false.
	if _, _, ok, err := s.DeletePlaylistCover(ctx, CoverScopePlaylist, "missing"); err != nil || ok {
		t.Errorf("delete missing: ok=%v err=%v", ok, err)
	}
}

func TestPrunePlaylistCoversExcept(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		_ = s.SetPlaylistCover(ctx, PlaylistCover{Scope: CoverScopeSmartMix, Key: k, ImageHash: k, UpdatedAt: 1})
	}
	removed, err := s.PrunePlaylistCoversExcept(ctx, CoverScopeSmartMix, map[string]struct{}{"b": {}})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("expected 2 removed, got %d (%+v)", len(removed), removed)
	}
	left, _ := s.PlaylistCoversByScope(ctx, CoverScopeSmartMix)
	if len(left) != 1 {
		t.Errorf("expected 1 survivor, got %+v", left)
	}
	if _, ok := left["b"]; !ok {
		t.Error("kept key 'b' was pruned")
	}
}

func TestSanitizeCoverKey(t *testing.T) {
	cases := map[string]string{
		"heavy-rotation":                       "heavy-rotation",
		"550e8400-e29b-41d4-a716-446655440000": "550e8400-e29b-41d4-a716-446655440000",
		"../../etc/passwd":                     ".._.._etc_passwd",
		"a/b":                                  "a_b",
		"":                                     "_",
	}
	for in, want := range cases {
		if got := SanitizeCoverKey(in); got != want {
			t.Errorf("SanitizeCoverKey(%q) = %q; want %q", in, got, want)
		}
	}
}
