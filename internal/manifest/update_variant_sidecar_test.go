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
