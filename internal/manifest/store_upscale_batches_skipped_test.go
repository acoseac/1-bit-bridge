package manifest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// findUpscaleBatchByID is a test helper that locates a freshly-inserted
// batch in ListUpscaleBatches output. Splitting it out keeps the
// driving test bodies below the per-method cognitive-complexity
// threshold SonarCloud enforces on this repo (caught by CodeRabbit
// major on PR #278's first push).
func findUpscaleBatchByID(t *testing.T, rows []UpscaleBatchRow, id uuid.UUID) UpscaleBatchRow {
	t.Helper()
	for _, r := range rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("row %s not in ListUpscaleBatches output", id)
	return UpscaleBatchRow{}
}

// TestInsertUpscaleBatch_SkippedFilesRoundTrip pins the v9 migration's
// `skipped_files` column round-trips correctly through InsertUpscaleBatch
// + ListUpscaleBatches. Distinct from the legacy round-trip test which
// reads via raw SQL on the pre-v9 column set.
func TestInsertUpscaleBatch_SkippedFilesRoundTrip(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	cases := []struct {
		name    string
		skipped int
	}{
		{"zero (pre-v9-shape compatibility)", 0},
		{"single skip", 1},
		{"many skips", 1234},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.Must(uuid.NewRandom())
			row := UpscaleBatchRow{
				ID:             id,
				Path:           "Music/Album/" + tc.name,
				TargetRate:     192000,
				TargetBits:     24,
				Status:         "completed",
				TotalFiles:     5,
				ProcessedFiles: 5,
				FailedFiles:    0,
				SkippedFiles:   tc.skipped,
				CreatedAt:      time.Now().UnixNano(),
				UpdatedAt:      time.Now().UnixNano(),
			}
			if err := s.InsertUpscaleBatch(context.Background(), row); err != nil {
				t.Fatalf("InsertUpscaleBatch: %v", err)
			}
			out, err := s.ListUpscaleBatches(context.Background(), 100)
			if err != nil {
				t.Fatalf("ListUpscaleBatches: %v", err)
			}
			got := findUpscaleBatchByID(t, out, id)
			if got.SkippedFiles != tc.skipped {
				t.Errorf("SkippedFiles round-trip: got %d, want %d", got.SkippedFiles, tc.skipped)
			}
		})
	}
}

// TestInsertUpscaleBatch_SkippedFilesDefaultsToZero confirms the
// pre-migration row shape (no SkippedFiles set on the struct, so
// it's the Go zero value) round-trips as 0 — back-compat for code
// paths that construct UpscaleBatchRow without setting the new
// field.
func TestInsertUpscaleBatch_SkippedFilesDefaultsToZero(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	id := uuid.Must(uuid.NewRandom())
	row := UpscaleBatchRow{
		ID:         id,
		Path:       "Music/Album/old-shape",
		TargetRate: 192000,
		TargetBits: 24,
		Status:     "completed",
		TotalFiles: 5,
		CreatedAt:  time.Now().UnixNano(),
		UpdatedAt:  time.Now().UnixNano(),
		// SkippedFiles deliberately omitted — Go zero value
	}
	if err := s.InsertUpscaleBatch(context.Background(), row); err != nil {
		t.Fatalf("InsertUpscaleBatch: %v", err)
	}
	out, err := s.ListUpscaleBatches(context.Background(), 100)
	if err != nil {
		t.Fatalf("ListUpscaleBatches: %v", err)
	}
	got := findUpscaleBatchByID(t, out, id)
	if got.SkippedFiles != 0 {
		t.Errorf("SkippedFiles default: got %d, want 0", got.SkippedFiles)
	}
}
