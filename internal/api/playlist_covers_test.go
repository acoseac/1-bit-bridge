package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestServeCover_presentAndMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{dir}, DataDir: dir, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")

	if err := os.MkdirAll(manifest.PlaylistCoverDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	jpegMagic := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if err := os.WriteFile(manifest.PlaylistCoverPath(dir, manifest.CoverScopeSmartMix, "heavy-rotation", "jpg"), jpegMagic, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := New(cfg, store, nil, "fp")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	// Present → 200 image/jpeg; the ?h= cache-buster is ignored.
	resp := authedGET(t, hs.URL+"/v1/smart-playlist-image/heavy-rotation?h=abc123", raw)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("present cover status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 4 || body[0] != 0xFF {
		t.Errorf("served body wrong: %x", body)
	}

	// Missing → 404 (iOS falls back to the auto-mosaic).
	resp2 := authedGET(t, hs.URL+"/v1/playlist-image/no-such-id", raw)
	defer resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Errorf("missing cover should 404; got %d", resp2.StatusCode)
	}
}

func TestSmartPlaylistsDTO_advertisesImageHash(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{dir}, DataDir: dir, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")

	mstore, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mstore.Close() })
	ctx := context.Background()
	if err := mstore.ReplaceSmartPlaylists(ctx, []manifest.StoredSmartPlaylist{
		{Slug: "heavy-rotation", Kind: "heavyRotation", Title: "Heavy Rotation", Position: 0, RefreshedAt: 100, ItemsJSON: []byte(`[{"position":0,"path":"/a.flac"}]`)},
		{Slug: "auto-mix", Kind: "autoMix", Title: "Auto Mix", Position: 1, RefreshedAt: 100, ItemsJSON: []byte(`[{"position":0,"path":"/b.flac"}]`)},
	}); err != nil {
		t.Fatal(err)
	}
	// A cover only for heavy-rotation.
	if err := mstore.SetPlaylistCover(ctx, manifest.PlaylistCover{
		Scope: manifest.CoverScopeSmartMix, Key: "heavy-rotation", ImageHash: "deadbeef", UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	srv := New(cfg, store, nil, "fp").
		WithSmartPlaylistStore(mstore).
		WithPlaylistCoverStore(mstore)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authedGET(t, hs.URL+"/v1/smart-playlists", raw)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("smart-playlists status = %d", resp.StatusCode)
	}
	var out struct {
		Playlists []struct {
			Slug      string `json:"slug"`
			ImageHash string `json:"imageHash"`
		} `json:"playlists"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	hashes := map[string]string{}
	for _, p := range out.Playlists {
		hashes[p.Slug] = p.ImageHash
	}
	if hashes["heavy-rotation"] != "deadbeef" {
		t.Errorf("heavy-rotation imageHash = %q; want deadbeef", hashes["heavy-rotation"])
	}
	if hashes["auto-mix"] != "" {
		t.Errorf("auto-mix should have no imageHash; got %q", hashes["auto-mix"])
	}
}

// serveCover must normalise the key the same way getPlaylist /
// putPlaylist / deletePlaylist do.
//
// The on-disk name is sha256(scope + "\x00" + key) — byte-exact — and
// GetPlaylistCover has no COLLATE NOCASE, so case reaches the
// filesystem. Swift's UUID().uuidString is UPPERCASE, so a client that
// built the image URL from its locally-generated id rather than the
// round-tripped DTO id got 200 from /v1/playlists/{ID} and 404 from
// /v1/playlist-image/{ID} — a confusing split with no diagnostic.
//
// The admin write paths (uploadCover / deleteCover) normalise in
// lockstep; doing only one side would orphan existing files.
func TestServeCover_normalisesKeyCase(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{dir}, DataDir: dir, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := store.Mint("probe")

	if err := os.MkdirAll(manifest.PlaylistCoverDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	// Stored under the canonical lowercase id, as putPlaylist writes it.
	const id = "b3c1f2a4-5d6e-7f80-9a1b-2c3d4e5f6071"
	jpegMagic := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if err := os.WriteFile(
		manifest.PlaylistCoverPath(dir, manifest.CoverScopePlaylist, id, "jpg"),
		jpegMagic, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := New(cfg, store, nil, "fp")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	// Uppercase — the shape Swift's UUID().uuidString produces.
	resp := authedGET(t, hs.URL+"/v1/playlist-image/B3C1F2A4-5D6E-7F80-9A1B-2C3D4E5F6071", raw)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("uppercase id status = %d, want 200 — the cover exists under the "+
			"canonical lowercase key and every sibling playlist endpoint normalises",
			resp.StatusCode)
	}
}
