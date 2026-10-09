package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/urlquery"
)

// reapDownload is one served library whose rendition row names a sidecar
// that is not on disk, with a variants directory the mount probe reads as
// holding a rendition, so the download reaches the reactive reap.
type reapDownload struct {
	store   *manifest.Store
	url     string
	token   string
	source  string
	variant string
	oldPath string
	newPath string
}

func newReapDownload(t *testing.T) *reapDownload {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	variants := filepath.Join(dir, "variants")
	d := &reapDownload{
		source:  "Artist/Album/01.flac",
		variant: "upscaled-v2-176400-24",
		oldPath: filepath.Join(dir, "old", "01.flac.upscaled-v2-176400-24.flac"),
		newPath: filepath.Join(dir, "moved", "01.flac.upscaled-v2-176400-24.flac"),
	}
	abs := filepath.Join(lib, d.source)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("source-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	// A rendition-named file so the probe calls the directory mounted.
	// The row's own sidecar is elsewhere and is not created.
	if err := os.MkdirAll(variants, 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(variants, "decoy.flac.upscaled-v2-176400-24.flac")
	if err := os.WriteFile(decoy, []byte("rendition"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d.store = store
	ctx := context.Background()
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: d.source, Size: info.Size(), ModTime: info.ModTime(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertVariant(ctx, manifest.VariantRow{
		SourcePath: d.source, VariantID: d.variant, SidecarPath: d.oldPath,
		Format: "flac", SizeBytes: 4242,
		SourceMTimeNS: info.ModTime().UnixNano(), SourceSize: info.Size(),
		CreatedAt: 9_000_001,
	}); err != nil {
		t.Fatal(err)
	}

	tokens, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := tokens.Mint("reap")
	if err != nil {
		t.Fatal(err)
	}
	d.token = token
	provider := manifest.NewProvider(store, nil)
	variantsDir := func() string { return variants }
	srv := api.New(&config.Config{LibraryRoots: []string{lib}}, tokens, provider, "reap-fingerprint").
		WithUpscale(func() bool { return true }, &variantStoreAdapter{
			provider: provider, store: store, variantsDir: variantsDir,
		}).
		WithVariantDeleter(&variantDeleterAdapter{store: store, variantsDir: variantsDir})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	d.url = hs.URL
	return d
}

func (d *reapDownload) get(t *testing.T) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		d.url+"/v1/download?path="+urlquery.Escape(d.source)+"&variant="+urlquery.Escape(d.variant), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// TestADownloadWhoseRowMovesBeforeTheReapKeepsIt is the window the
// compare-and-delete of a listed row does not cover: the download looks
// the row up, a move rewrites it, then the open of the path captured at
// lookup fails and the reap deletes by that snapshot. The moved row stays,
// and the client still gets the 410 for the file this request could not open.
func TestADownloadWhoseRowMovesBeforeTheReapKeepsIt(t *testing.T) {
	d := newReapDownload(t)
	t.Cleanup(func() { api.SetBeforeVariantReapForTest(nil) })
	api.SetBeforeVariantReapForTest(func(rec api.VariantRecord) {
		if rec.SourcePath != d.source || rec.VariantID != d.variant {
			return
		}
		if err := d.store.UpdateVariantSidecarPath(context.Background(), rec.SourcePath, rec.VariantID, d.newPath); err != nil {
			t.Errorf("rewrite: %v", err)
		}
	})
	status, body := d.get(t)
	if status != http.StatusGone || !strings.Contains(body, "variant_missing_on_disk") {
		t.Fatalf("status %d body %s", status, body)
	}
	got, err := d.store.GetVariant(context.Background(), d.source, d.variant)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.SidecarPath != d.newPath {
		t.Fatalf("moved row: %+v", got)
	}
}

// TestADownloadOfAMissingSidecarStillDropsTheRow is the reap's own case:
// the file is gone and the row is still the one the download looked up, so
// the row goes and the client gets the same 410.
func TestADownloadOfAMissingSidecarStillDropsTheRow(t *testing.T) {
	d := newReapDownload(t)
	status, body := d.get(t)
	if status != http.StatusGone || !strings.Contains(body, "variant_missing_on_disk") {
		t.Fatalf("status %d body %s", status, body)
	}
	got, err := d.store.GetVariant(context.Background(), d.source, d.variant)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("the missing sidecar's row was kept: %+v", got)
	}
}
