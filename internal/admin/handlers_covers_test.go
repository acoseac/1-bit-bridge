package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func tinyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProcessCoverImage(t *testing.T) {
	// A 1000×800 PNG is resized so the longest side == coverMaxDim, re-encoded
	// as JPEG, and hashed.
	out, hash, err := processCoverImage(tinyPNG(t, 1000, 800))
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if hash == "" {
		t.Error("empty hash")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || format != "jpeg" {
		t.Fatalf("output not jpeg: format=%q err=%v", format, err)
	}
	if cfg.Width != coverMaxDim {
		t.Errorf("longest side not resized to %d: got %dx%d", coverMaxDim, cfg.Width, cfg.Height)
	}
	if cfg.Height >= cfg.Width {
		t.Errorf("aspect not preserved (want landscape): %dx%d", cfg.Width, cfg.Height)
	}

	// A small image is NOT upscaled.
	out2, _, err := processCoverImage(tinyPNG(t, 120, 120))
	if err != nil {
		t.Fatalf("process small: %v", err)
	}
	c2, _, _ := image.DecodeConfig(bytes.NewReader(out2))
	if c2.Width != 120 || c2.Height != 120 {
		t.Errorf("small image was rescaled: %dx%d", c2.Width, c2.Height)
	}

	// Non-image bytes rejected.
	if _, _, err := processCoverImage([]byte("not an image at all, just text")); err == nil {
		t.Error("expected error for non-image input")
	}
}

func TestUploadAndDeleteCover(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	dataDir := srv.deps.CfgHolder.Load().DataDir
	slug := "heavy-rotation"

	// Upload a PNG → normalized JPEG on disk + mapping recorded.
	b64 := base64.StdEncoding.EncodeToString(tinyPNG(t, 400, 400))
	var up struct {
		ImageHash string `json:"imageHash"`
	}
	code := doJSON(t, h, "POST", "/api/smart-playlists/"+slug+"/cover",
		map[string]any{"image": b64}, &up)
	if code != 200 {
		t.Fatalf("upload: %d", code)
	}
	if up.ImageHash == "" {
		t.Error("upload response missing imageHash")
	}

	coverPath := manifest.PlaylistCoverPath(dataDir, manifest.CoverScopeSmartMix, slug, "jpg")
	fileBytes, err := os.ReadFile(coverPath)
	if err != nil {
		t.Fatalf("cover file not written: %v", err)
	}
	if _, format, _ := image.DecodeConfig(bytes.NewReader(fileBytes)); format != "jpeg" {
		t.Errorf("stored cover not jpeg: %q", format)
	}
	got, ok, _ := srv.deps.Manifest.GetPlaylistCover(context.Background(), manifest.CoverScopeSmartMix, slug)
	if !ok || got.ImageHash != up.ImageHash {
		t.Errorf("mapping not recorded / hash mismatch: ok=%v got=%q want=%q", ok, got.ImageHash, up.ImageHash)
	}

	// Reject a non-image upload.
	bad := base64.StdEncoding.EncodeToString([]byte("definitely not an image payload"))
	code = doJSON(t, h, "POST", "/api/smart-playlists/"+slug+"/cover", map[string]any{"image": bad}, &struct{}{})
	if code != 400 {
		t.Errorf("non-image upload should 400; got %d", code)
	}

	// Delete → mapping gone + file removed.
	code = doJSON(t, h, "DELETE", "/api/smart-playlists/"+slug+"/cover", nil, &struct{}{})
	if code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if _, ok, _ := srv.deps.Manifest.GetPlaylistCover(context.Background(), manifest.CoverScopeSmartMix, slug); ok {
		t.Error("mapping still present after delete")
	}
	if _, err := os.Stat(coverPath); !os.IsNotExist(err) {
		t.Errorf("cover file still on disk after delete: %v", err)
	}
}
