package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// sentinelVariantStore answers every lookup with the same record or
// error — the shape cmd/bridge's variantStoreAdapter produces for a row
// whose sidecar is present at the canonical location but not yet whole.
type sentinelVariantStore struct {
	rec *VariantRecord
	err error
}

func (s *sentinelVariantStore) LookupVariant(context.Context, string, string) (*VariantRecord, error) {
	return s.rec, s.err
}

// reactiveFixture is fileVariantFixture with the reactive reaper WIRED —
// the piece every earlier variant download test left out, which is why
// none of them could say whether a 410 reaped the row.
func reactiveFixture(t *testing.T, vs VariantStore) (*httptest.Server, string, string, *stubVariantDeleter) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(filepath.Join(root, "Artist/Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Artist/Album/01.flac"), make([]byte, 256), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{root}, ListenAddress: ":7788", LibraryName: "Test"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")
	deleter := &stubVariantDeleter{}
	srv := New(cfg, store, nil, "fp").WithUpscale(func() bool { return true }, vs).WithVariantDeleter(deleter)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, root, deleter
}

// TestServeVariantKeepsTheRowWhenTheStoreSaysUnavailable pins the
// serve-side half of the relocation contract: ErrVariantSidecarUnavailable
// is the client's familiar 410 variant_missing_on_disk WITHOUT the reap.
// The positive control in the second half — a record whose file is gone
// at the path the store hands over — shows the same fixture reaping, so
// the first half's "not called" is a decision and not a blind spot.
func TestServeVariantKeepsTheRowWhenTheStoreSaysUnavailable(t *testing.T) {
	t.Run("unavailable: 410, row kept", func(t *testing.T) {
		vs := &sentinelVariantStore{err: fmt.Errorf("copy in flight: %w", ErrVariantSidecarUnavailable)}
		hs, tok, _, deleter := reactiveFixture(t, vs)
		resp := authGet(t, hs, "/v1/download?path=Artist/Album/01.flac&variant=upscaled-v2-176400-24", tok)
		defer resp.Body.Close()
		if resp.StatusCode != 410 {
			t.Fatalf("status = %d, want 410", resp.StatusCode)
		}
		assertWireErrorCode(t, resp, "variant_missing_on_disk")
		if got := deleter.deletedKeys(); len(got) != 0 {
			t.Fatalf("the reactive reaper deleted %v for a row the store said to keep", got)
		}
	})
	t.Run("control: missing at the served path reaps", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "gone.flac")
		vs := &sentinelVariantStore{rec: &VariantRecord{
			SourcePath: "Artist/Album/01.flac", VariantID: "upscaled-v2-176400-24", SidecarPath: gone,
		}}
		hs, tok, root, deleter := reactiveFixture(t, vs)
		// The freshness gate compares the record's source facts to the
		// file; a zero record reads as "drifted", so fill them from disk.
		info, err := os.Stat(filepath.Join(root, "Artist/Album/01.flac"))
		if err != nil {
			t.Fatal(err)
		}
		vs.rec.SourceMTimeNS, vs.rec.SourceSize = info.ModTime().UnixNano(), info.Size()
		resp := authGet(t, hs, "/v1/download?path=Artist/Album/01.flac&variant=upscaled-v2-176400-24", tok)
		defer resp.Body.Close()
		if resp.StatusCode != 410 {
			t.Fatalf("status = %d, want 410", resp.StatusCode)
		}
		assertWireErrorCode(t, resp, "variant_missing_on_disk")
		if got := deleter.deletedKeys(); len(got) != 1 || got[0] != "Artist/Album/01.flac|upscaled-v2-176400-24" {
			t.Fatalf("control: the reactive reaper should have deleted the row, got %v", got)
		}
	})
}
