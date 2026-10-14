package integrity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// storeVariantLister is the sweep's view of a real store: the five
// columns VariantSnapshot carries, read from AllVariants.
type storeVariantLister struct {
	store *manifest.Store
}

func (l storeVariantLister) AllVariants(ctx context.Context) ([]VariantSnapshot, error) {
	rows, err := l.store.AllVariants(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]VariantSnapshot, len(rows))
	for i, row := range rows {
		out[i] = VariantSnapshot{
			SourcePath: row.SourcePath, VariantID: row.VariantID,
			SidecarPath: row.SidecarPath, SizeBytes: row.SizeBytes, CreatedAt: row.CreatedAt,
		}
	}
	return out, nil
}

// seedClearMissingRendition writes one filesystem track and one rendition
// file under variants, and returns the rendition path.
func seedClearMissingRendition(t *testing.T, s *manifest.Store, variants, path string, missing bool) string {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertTrack(ctx, &manifest.Track{Path: path, Size: 1000, Codec: "FLAC"}); err != nil {
		t.Fatal(err)
	}
	side := filepath.Join(variants, filepath.Base(path)+".upscaled-v2-192000-24.flac")
	if err := os.WriteFile(side, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVariant(ctx, manifest.VariantRow{
		SourcePath: path, VariantID: "upscaled-v2-192000-24",
		SidecarPath: side, Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1,
		SourceMTimeNS: 1, SourceSize: 1000, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if missing {
		n, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{path}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("increment deleted %d, want 0 (threshold 100)", n)
		}
	}
	return side
}

func openClearMissingStore(t *testing.T) *manifest.Store {
	t.Helper()
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestClearMissingLeavesNoRenditionForTheOrphanSweep: after clear-missing,
// the rendition files of the cleared tracks are gone, so one orphan tick
// neither refuses nor unlinks. Two shapes, because the sweep refuses them
// differently when the files are left behind: a leftover tree beside one
// remaining rendition is the lost-index refusal, and a leftover with no
// variant row left is the empty-catalog refusal.
func TestClearMissingLeavesNoRenditionForTheOrphanSweep(t *testing.T) {
	t.Run("beside a remaining rendition", func(t *testing.T) {
		rec := loggingtest.Record(t)
		s := openClearMissingStore(t)
		variants := t.TempDir()
		kept := seedClearMissingRendition(t, s, variants, "keep/kept.flac", false)
		var doomed []string
		for i := 0; i < 12; i++ {
			doomed = append(doomed, seedClearMissingRendition(t, s, variants,
				fmt.Sprintf("gone/track-%02d.flac", i), true))
		}
		if _, err := s.ClearMissingCounts(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, p := range doomed {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("cleared rendition %s still on disk (err=%v)", p, err)
			}
		}
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("kept rendition: %v", err)
		}
		sw := NewOrphanSidecarSweeper(storeVariantLister{s}, staticDir(variants), time.Hour, sweepPercent)
		if n := sw.tick(context.Background()); n != 0 {
			t.Errorf("unlinked %d, want 0", n)
		}
		if got := rec.Failures(msgOrphanRefusal, msgOrphanEmptyCatalog); len(got) != 0 {
			t.Errorf("sweep refused: %v", got)
		}
	})

	t.Run("when no variant row remains", func(t *testing.T) {
		rec := loggingtest.Record(t)
		s := openClearMissingStore(t)
		variants := t.TempDir()
		doomed := seedClearMissingRendition(t, s, variants, "gone/01.flac", true)
		if _, err := s.ClearMissingCounts(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(doomed); !os.IsNotExist(err) {
			t.Errorf("cleared rendition still on disk (err=%v)", err)
		}
		sw := NewOrphanSidecarSweeper(storeVariantLister{s}, staticDir(variants), time.Hour, sweepPercent)
		if n := sw.tick(context.Background()); n != 0 {
			t.Errorf("unlinked %d, want 0", n)
		}
		if got := rec.Failures(msgOrphanRefusal, msgOrphanEmptyCatalog); len(got) != 0 {
			t.Errorf("sweep refused: %v", got)
		}
	})
}
