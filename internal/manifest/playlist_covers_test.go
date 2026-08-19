package manifest

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
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

// TestPlaylistCoverFilenameInjective pins the fix for the lossy filename
// scheme: the pre-fix SanitizeCoverKey form mapped "a b" and "a_b" to the
// SAME basename (space → '_'), so a cover upload for one silently
// overwrote the other's while serveCover advertised the correct imageHash.
// The sha256 identity must keep distinct (scope, key) pairs on distinct
// files while staying traversal-safe.
func TestPlaylistCoverFilenameInjective(t *testing.T) {
	fnAB := PlaylistCoverFilename(CoverScopePlaylist, "a b", "jpg")
	fnAUnderB := PlaylistCoverFilename(CoverScopePlaylist, "a_b", "jpg")
	if fnAB == fnAUnderB {
		t.Fatalf("collision: keys \"a b\" and \"a_b\" both map to %q", fnAB)
	}

	// Deterministic — the same identity always resolves to the same file.
	if PlaylistCoverFilename(CoverScopePlaylist, "a b", "jpg") != fnAB {
		t.Error("PlaylistCoverFilename is not deterministic")
	}

	// Scope is part of the identity: same key under different scopes must
	// not collide.
	if PlaylistCoverFilename(CoverScopeSmartMix, "x", "jpg") ==
		PlaylistCoverFilename(CoverScopePlaylist, "x", "jpg") {
		t.Error("scope must be part of the filename identity")
	}

	// The hash alphabet is pure hex, so no key can inject a path separator
	// or traversal sequence; the extension stays the only structural part.
	for _, key := range []string{"../../etc/passwd", "a/b", "a b", "..", "spaces & symbols!"} {
		fn := PlaylistCoverFilename(CoverScopePlaylist, key, "jpg")
		if strings.ContainsAny(fn, `/\`) || strings.Contains(fn, "..") {
			t.Errorf("filename %q for key %q contains a path separator / traversal", fn, key)
		}
		if !strings.HasSuffix(fn, ".jpg") {
			t.Errorf("filename %q for key %q missing .jpg extension", fn, key)
		}
	}
}

// TestPlaylistCoverPathIsolatesDistinctKeys proves an on-disk
// upload/serve/delete round-trip for one key never touches a
// collision-prone sibling's file (pre-fix "a b" and "a_b" shared a path).
func TestPlaylistCoverPathIsolatesDistinctKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(PlaylistCoverDir(dir), 0o700); err != nil {
		t.Fatalf("mkdir covers: %v", err)
	}
	pathAB := PlaylistCoverPath(dir, CoverScopePlaylist, "a b", "jpg")
	pathAUnderB := PlaylistCoverPath(dir, CoverScopePlaylist, "a_b", "jpg")
	if pathAB == pathAUnderB {
		t.Fatalf("distinct keys share a path: %q", pathAB)
	}

	// "Upload" a cover for "a b".
	if err := os.WriteFile(pathAB, []byte("cover-for-a-space-b"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The sibling key "a_b" must NOT resolve to the file we just wrote.
	if _, err := os.Stat(pathAUnderB); !os.IsNotExist(err) {
		t.Fatalf("sibling key \"a_b\" unexpectedly resolves to an existing file (err=%v)", err)
	}
	// "Serve" for "a b" reads back the right bytes.
	got, err := os.ReadFile(pathAB)
	if err != nil || string(got) != "cover-for-a-space-b" {
		t.Fatalf("serve read: got %q err %v", got, err)
	}
	// "Delete" for "a b" removes its own file and never touched "a_b".
	if err := os.Remove(pathAB); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(pathAB); !os.IsNotExist(err) {
		t.Error("file for \"a b\" still present after delete")
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

// TestPrunePlaylistCoversExceptStaysUnwired pins the 2026-08-19 decision that
// this primitive has NO automatic caller.
//
// Both triggers it was written for are unsafe: smart-mix retirement is
// reversible (a family below its floor returns later), and playlist deletion
// is a tombstone whose id can be revived by a newer-clock upsert. Covers are
// operator-uploaded, so an automatic prune silently destroys authored content
// to reclaim a JPEG.
//
// The obvious "fix" for a reviewer who finds unwired-but-tested code is to
// wire it. This is the guard that makes that a deliberate act instead of a
// tidy-up: it fails the moment a production caller appears, pointing at the
// docblock.
func TestPrunePlaylistCoversExceptStaysUnwired(t *testing.T) {
	// AST, not a substring scan over the bytes: the name appears in PROSE in
	// this package (its own docblock explains why it is unwired, and so does
	// this test), and a future comment elsewhere saying "deliberately does not
	// call PrunePlaylistCoversExcept" would fail a text search. A guard that
	// cries wolf gets deleted — which is the outcome this exists to prevent.
	// (gemini-code-assist on PR #725.)
	roots := []string{"..", "../../cmd"}
	fset := token.NewFileSet()
	var callers []string
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return nil //nolint:nilerr // unreadable trees just aren't scanned
			}
			// Tests may reference it (the round-trip test calls it directly);
			// playlist_covers.go is the DECLARATION site.
			if strings.HasSuffix(p, "_test.go") || strings.HasSuffix(p, "playlist_covers.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				return nil //nolint:nilerr // unparseable files aren't callers
			}
			found := false
			ast.Inspect(file, func(n ast.Node) bool {
				if found {
					return false
				}
				if id, ok := n.(*ast.Ident); ok && id.Name == "PrunePlaylistCoversExcept" {
					found = true
					return false
				}
				return true
			})
			if found {
				callers = append(callers, p)
			}
			return nil
		})
	}
	if len(callers) > 0 {
		t.Errorf("PrunePlaylistCoversExcept gained production caller(s) %v.\n"+
			"It is deliberately unwired — see its docblock. Smart-mix retirement is REVERSIBLE "+
			"(a family below MinFavorites returns later) and playlist deletion is a revivable "+
			"tombstone, so an automatic prune deletes operator-uploaded artwork that should "+
			"still be there. If this call is intentional, it needs a keep-set that is "+
			"authoritative and exclusions that are permanent — update the docblock and this test "+
			"together.", callers)
	}
}
