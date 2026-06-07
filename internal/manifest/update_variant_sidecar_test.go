package manifest

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestUpdateVariantSidecarPathRoundTrip confirms the path-only
// update lands cleanly: row content elsewhere is unchanged; the
// new path is readable on the next GetVariant.
func TestUpdateVariantSidecarPathRoundTrip(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	// Need a parent track row for the FK.
	if err := s.UpsertTrack(ctx, &Track{
		Path: filepath.Join("Album", "Track.flac"),
		Size: 100, ModTime: now,
	}); err != nil {
		t.Fatal(err)
	}
	v := VariantRow{
		SourcePath:    filepath.Join("Album", "Track.flac"),
		VariantID:     "upscaled-v2-176400-24",
		SidecarPath:   "/old/path/Track.upscaled-v2-176400-24.flac",
		Format:        "flac",
		SampleRate:    176400,
		BitsPerSample: 24,
		SizeBytes:     1024,
		CreatedAt:     now.UnixNano(),
	}
	if err := s.UpsertVariant(ctx, v); err != nil {
		t.Fatal(err)
	}

	newPath := "/new/path/Track.upscaled-v2-176400-24.flac"
	if err := s.UpdateVariantSidecarPath(ctx, v.SourcePath, v.VariantID, newPath); err != nil {
		t.Fatal(err)
	}

	// Read back via GetVariant — sidecar_path must reflect the new
	// value; everything else must be unchanged.
	got, err := s.GetVariant(ctx, v.SourcePath, v.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SidecarPath != newPath {
		t.Errorf("sidecar_path: got %q, want %q", got.SidecarPath, newPath)
	}
	if got.SampleRate != v.SampleRate || got.BitsPerSample != v.BitsPerSample {
		t.Errorf("rate/bits drifted: got %d/%d, want %d/%d",
			got.SampleRate, got.BitsPerSample, v.SampleRate, v.BitsPerSample)
	}
	if got.SizeBytes != v.SizeBytes {
		t.Errorf("size_bytes drifted: got %d, want %d", got.SizeBytes, v.SizeBytes)
	}
}

// TestCountVariantsNotUnderPrefix confirms the SQL-aggregate
// replacement for the prior in-Go AllVariants walk: variants
// under the prefix are excluded, anything elsewhere is counted.
//
// Trailing-separator requirement: `/tmp/new` is a sibling of
// `/tmp/new2`; callers must pass a trailing separator so the LIKE
// pattern doesn't false-match a co-named sibling directory.
func TestCountVariantsNotUnderPrefix(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, name := range []string{"A.flac", "B.flac", "C.flac"} {
		if err := s.UpsertTrack(ctx, &Track{
			Path: filepath.Join("Album", name), Size: 100, ModTime: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	fixtures := []VariantRow{
		{SourcePath: filepath.Join("Album", "A.flac"), VariantID: "upscaled-v2-176400-24",
			SidecarPath: "/tmp/new/Album/A.flac.upscaled-v2-176400-24.flac",
			Format:      "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 100, CreatedAt: now.UnixNano()},
		{SourcePath: filepath.Join("Album", "B.flac"), VariantID: "upscaled-v2-176400-24",
			SidecarPath: "/tmp/new/Album/B.flac.upscaled-v2-176400-24.flac",
			Format:      "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 200, CreatedAt: now.UnixNano()},
		{SourcePath: filepath.Join("Album", "C.flac"), VariantID: "upscaled-v2-176400-24",
			SidecarPath: "/tmp/old/Album/C.flac.upscaled-v2-176400-24.flac",
			Format:      "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 50, CreatedAt: now.UnixNano()},
	}
	for _, v := range fixtures {
		if err := s.UpsertVariant(ctx, v); err != nil {
			t.Fatal(err)
		}
	}

	count, bytes, err := s.CountVariantsNotUnderPrefix(ctx, "/tmp/new/")
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || bytes != 50 {
		t.Errorf("not under /tmp/new/: got (%d, %d), want (1, 50)", count, bytes)
	}

	count, bytes, err = s.CountVariantsNotUnderPrefix(ctx, "/tmp/old/")
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || bytes != 300 {
		t.Errorf("not under /tmp/old/: got (%d, %d), want (2, 300)", count, bytes)
	}

	// Empty prefix → total (every variant is "not under empty"), per the
	// documented contract. (DeepSeek review — impl previously returned 0.)
	count, bytes, err = s.CountVariantsNotUnderPrefix(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || bytes != 350 {
		t.Errorf("empty prefix: got (%d, %d), want (3, 350)", count, bytes)
	}
}

// TestUpdateVariantSidecarPathMissingRow returns sql.ErrNoRows
// wrapped so callers can distinguish "row not found" from a transient
// driver error. The CLI move pipeline relies on this to skip
// orphan-in-DB-only rows with a warning.
func TestUpdateVariantSidecarPathMissingRow(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	err := s.UpdateVariantSidecarPath(ctx, "Album/Missing.flac", "upscaled-v2-176400-24", "/new/path.flac")
	if err == nil {
		t.Fatalf("expected error for missing row, got nil")
	}
	// The helper wraps sql.ErrNoRows so callers can branch on it.
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows wrapped, got %v", err)
	}
}
