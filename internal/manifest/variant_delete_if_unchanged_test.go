package manifest

import (
	"context"
	"errors"
	"testing"
	"time"
)

// DeleteVariantIfUnchanged is the VariantWatcher's delete (backlog B204): it
// removes a variant row only while the row still records the sidecar path,
// size and created_at the caller listed it with, so a row another writer
// changed after the listing is never deleted on the strength of it. The
// writers are the ones this table has: UpdateVariantSidecarPath (`bridge
// variants move`, an adoption), UpsertVariant (a render) and DeleteVariant.

// listedVariant seeds a track with one variant row and returns the row as
// AllVariants lists it, the shape the watcher hands the delete.
func listedVariant(t *testing.T, s *Store) VariantRow {
	t.Helper()
	ctx := context.Background()
	upsertParent(t, s, "Music/A/1.flac")
	if err := s.UpsertVariant(ctx, VariantRow{
		SourcePath: "Music/A/1.flac", VariantID: "upscaled-v2-176400-24",
		SidecarPath: "/srv/variants/Music/A/1.flac.upscaled-v2-176400-24.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 100,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1_000,
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}
	rows, err := s.AllVariants(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("AllVariants: %d row(s), err %v", len(rows), err)
	}
	return rows[0]
}

// parentIndexedAt reads the parent track's indexed_at.
func parentIndexedAt(t *testing.T, s *Store) int64 {
	t.Helper()
	var at int64
	if err := s.db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac").Scan(&at); err != nil {
		t.Fatalf("read indexed_at: %v", err)
	}
	return at
}

// TestDeleteVariantIfUnchangedDeletesARowStillAsListed — the row nobody
// touched is deleted, and the parent's indexed_at advances, as DeleteVariant
// does, so a paired device sees the rendition go.
func TestDeleteVariantIfUnchangedDeletesARowStillAsListed(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	listed := listedVariant(t, s)
	before := parentIndexedAt(t, s)

	if err := s.DeleteVariantIfUnchanged(context.Background(), listed); err != nil {
		t.Fatalf("DeleteVariantIfUnchanged of the row as listed: %v", err)
	}
	if row, err := s.GetVariant(context.Background(), listed.SourcePath, listed.VariantID); err != nil || row != nil {
		t.Errorf("the row is still there (row %v, err %v)", row, err)
	}
	if after := parentIndexedAt(t, s); after <= before {
		t.Errorf("indexed_at %d after the delete, want past %d", after, before)
	}
}

// TestDeleteVariantIfUnchangedKeepsARowAnotherWriterChanged — each writer of
// the table, between the listing and the delete, and each column the
// comparison holds: the delete writes nothing, answers ErrVariantChanged, and
// leaves the parent's indexed_at alone. The render that rewrites the row at
// the same path with the same size (an unchanged source renders the same
// bytes) differs only in created_at.
func TestDeleteVariantIfUnchangedKeepsARowAnotherWriterChanged(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		change func(t *testing.T, s *Store, listed VariantRow)
	}{
		{"moved (bridge variants move, an adoption)", func(t *testing.T, s *Store, listed VariantRow) {
			if err := s.UpdateVariantSidecarPath(ctx, listed.SourcePath, listed.VariantID,
				"/mnt/new-disk/variants/Music/A/1.flac.upscaled-v2-176400-24.flac"); err != nil {
				t.Fatal(err)
			}
		}},
		// The size alone, created_at kept: no writer does that, and it is
		// here because the size is half of what the caller's verdict was
		// taken from (LocateSidecar), so the comparison holds it apart.
		{"rewritten at another size", func(t *testing.T, s *Store, listed VariantRow) {
			again := listed
			again.SizeBytes = 120
			if err := s.UpsertVariant(ctx, again); err != nil {
				t.Fatal(err)
			}
		}},
		{"re-rendered at the same path and size", func(t *testing.T, s *Store, listed VariantRow) {
			again := listed
			again.CreatedAt = listed.CreatedAt + 1
			if err := s.UpsertVariant(ctx, again); err != nil {
				t.Fatal(err)
			}
		}},
		{"deleted", func(t *testing.T, s *Store, listed VariantRow) {
			if err := s.DeleteVariant(ctx, listed.SourcePath, listed.VariantID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { requireDeleteRefusedAfter(t, c.change) })
	}
}

// requireDeleteRefusedAfter lists a row, lets change rewrite or remove it,
// and fails the test unless DeleteVariantIfUnchanged of the row as listed
// then answers ErrVariantChanged, leaves the row as change left it (or
// absent), and leaves the parent's indexed_at where it was.
func requireDeleteRefusedAfter(t *testing.T, change func(t *testing.T, s *Store, listed VariantRow)) {
	t.Helper()
	ctx := context.Background()
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	listed := listedVariant(t, s)
	change(t, s, listed)
	before := parentIndexedAt(t, s)
	want, err := s.GetVariant(ctx, listed.SourcePath, listed.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	// A clock that would move indexed_at if the delete bumped it.
	s.now = func() time.Time { return time.Unix(0, before+time.Hour.Nanoseconds()) }

	if err := s.DeleteVariantIfUnchanged(ctx, listed); !errors.Is(err, ErrVariantChanged) {
		t.Fatalf("DeleteVariantIfUnchanged after the row changed: err %v, want ErrVariantChanged", err)
	}
	got, err := s.GetVariant(ctx, listed.SourcePath, listed.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if (got == nil) != (want == nil) || (got != nil && *got != *want) {
		t.Errorf("the row is %+v after the delete, want it as the writer left it: %+v", got, want)
	}
	if after := parentIndexedAt(t, s); after != before {
		t.Errorf("indexed_at moved from %d to %d on a delete that wrote nothing", before, after)
	}
}
