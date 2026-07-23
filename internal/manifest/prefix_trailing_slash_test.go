package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// seedPrefixFixture writes two tracks under "Album/" so the prefix
// queries have something to find.
func seedPrefixFixture(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	rate := 44100.0
	bits := 16
	for _, name := range []string{"01 - One.flac", "02 - Two.flac"} {
		if err := s.UpsertTrack(context.Background(), &Track{
			Path:          "Album/" + name,
			Size:          1000,
			ModTime:       time.Unix(1700000000, 0).UTC(),
			Codec:         "FLAC",
			SampleRate:    &rate,
			BitsPerSample: &bits,
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return s
}

// TestPrefixQueriesTolerateTrailingSlash pins both halves of a bug class
// the codebase already fixed once, in RollupByPrefix, and then repeated
// in two siblings.
//
// Both queries append their own separator to the caller's prefix. Given
// "Album/" the byte-range form builds `path >= 'Album//'`, and since the
// first byte after "Album/" in a real row ('0'..'~') sorts above the '0'
// upper-bound sentinel, EVERY row falls outside the range. The LIKE form
// builds 'Album//%' and matches nothing for the same reason.
//
// The failure is silent — zero rows, no error — so it surfaces as an
// Inspector folder reporting "0 eligible" / a batch pre-flight reporting
// "nothing to do" for a folder full of work. Reachable in production:
// admin/handlers_library_inspector.go forwards req.Path verbatim,
// unlike api/upscale_batch.go which runs path.Clean first.
func TestPrefixQueriesTolerateTrailingSlash(t *testing.T) {
	s := seedPrefixFixture(t)
	ctx := context.Background()

	t.Run("EligibleRollupByPrefix", func(t *testing.T) {
		bare, err := s.EligibleRollupByPrefix(ctx, "Album", 96000, 24)
		if err != nil {
			t.Fatalf("bare prefix: %v", err)
		}
		slashed, err := s.EligibleRollupByPrefix(ctx, "Album/", 96000, 24)
		if err != nil {
			t.Fatalf("trailing-slash prefix: %v", err)
		}
		if bare.Upscale == 0 && bare.Optimize == 0 {
			t.Fatal("fixture seeded nothing eligible — test can't detect the bug")
		}
		if slashed != bare {
			t.Errorf("trailing slash changed the result: bare=%+v slashed=%+v", bare, slashed)
		}
	})

	t.Run("ListTrackProjectionsUnderPrefix", func(t *testing.T) {
		bare, err := s.ListTrackProjectionsUnderPrefix(ctx, "Album", VariantKindPrefixUpscaled)
		if err != nil {
			t.Fatalf("bare prefix: %v", err)
		}
		slashed, err := s.ListTrackProjectionsUnderPrefix(ctx, "Album/", VariantKindPrefixUpscaled)
		if err != nil {
			t.Fatalf("trailing-slash prefix: %v", err)
		}
		if len(bare) != 2 {
			t.Fatalf("bare prefix found %d projections, want 2 (fixture broken)", len(bare))
		}
		if len(slashed) != len(bare) {
			t.Errorf("trailing slash found %d projections, bare found %d — "+
				"the pre-flight would silently report nothing to do", len(slashed), len(bare))
		}
	})
}
