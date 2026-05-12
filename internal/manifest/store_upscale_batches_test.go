package manifest

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMigrationV6_TableExists verifies the upscale_batches table is
// created at the head of the migration ladder on a fresh DB. Indirectly
// asserts the CHECK constraint and the index landed without errors
// (a malformed DDL would have failed OpenStore).
func TestMigrationV6_TableExists(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	var name string
	err := s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='upscale_batches'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("upscale_batches table not found: %v", err)
	}
	if name != "upscale_batches" {
		t.Errorf("unexpected table name: %q", name)
	}

	// Index exists?
	var idxName string
	err = s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_upscale_batches_status_created'`,
	).Scan(&idxName)
	if err != nil {
		t.Errorf("idx_upscale_batches_status_created not found: %v", err)
	}
}

// TestMigrationV6_CheckConstraintRejectsInvalidStatus verifies the
// CHECK constraint at the SQL layer surfaces an error for any status
// outside the documented enum. Defense-in-depth: even if the
// coordinator's Go-side validation drifts, the DB refuses the row.
func TestMigrationV6_CheckConstraintRejectsInvalidStatus(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	row := UpscaleBatchRow{
		ID:         uuid.Must(uuid.NewRandom()),
		Path:       "Music/Album",
		TargetRate: 192000,
		TargetBits: 24,
		Status:     "garbage", // not in the enum
		CreatedAt:  time.Now().UnixNano(),
		UpdatedAt:  time.Now().UnixNano(),
	}
	if err := s.InsertUpscaleBatch(row); err == nil {
		t.Fatal("InsertUpscaleBatch with invalid status: want error, got nil")
	}
}

// TestInsertUpscaleBatch_RoundTrip pins the basic insert path against
// the valid status values. Reads back via raw SQL because PR 1 doesn't
// ship a GetUpscaleBatch helper (PR 3 introduces the full CRUD).
func TestInsertUpscaleBatch_RoundTrip(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	validStatuses := []string{
		"pending", "running", "completed",
		"failed", "cancelled", "interrupted",
	}
	for _, status := range validStatuses {
		t.Run(status, func(t *testing.T) {
			id := uuid.Must(uuid.NewRandom())
			row := UpscaleBatchRow{
				ID:             id,
				Path:           "Music/Album/" + status,
				TargetRate:     192000,
				TargetBits:     24,
				Status:         status,
				TotalFiles:     10,
				ProcessedFiles: 3,
				FailedFiles:    1,
				Error:          "",
				CreatedAt:      time.Now().UnixNano(),
				UpdatedAt:      time.Now().UnixNano(),
			}
			if err := s.InsertUpscaleBatch(row); err != nil {
				t.Fatalf("InsertUpscaleBatch(%q): %v", status, err)
			}

			// Read back via raw SQL to verify each column survived.
			var (
				gotPath           string
				gotRate, gotBits  int
				gotStatus         string
				gotTotal, gotProc int
				gotFailed         int
				gotError          *string
			)
			err := s.db.QueryRow(`
				SELECT path, target_rate, target_bits, status,
				       total_files, processed_files, failed_files, error
				FROM upscale_batches WHERE id = ?
			`, id[:]).Scan(
				&gotPath, &gotRate, &gotBits, &gotStatus,
				&gotTotal, &gotProc, &gotFailed, &gotError,
			)
			if err != nil {
				t.Fatalf("SELECT: %v", err)
			}
			if gotPath != row.Path || gotRate != row.TargetRate ||
				gotBits != row.TargetBits || gotStatus != row.Status ||
				gotTotal != row.TotalFiles || gotProc != row.ProcessedFiles ||
				gotFailed != row.FailedFiles {
				t.Errorf("row mismatch: got path=%q rate=%d bits=%d status=%q total=%d proc=%d failed=%d",
					gotPath, gotRate, gotBits, gotStatus,
					gotTotal, gotProc, gotFailed)
			}
		})
	}
}

// TestRecoverInterruptedBatches_TransitionsPendingAndRunning seeds rows
// in every status, runs the recovery helper, and asserts only `pending`
// and `running` are flipped to `interrupted`. Terminal-status rows must
// not be touched (a `completed` batch must not be re-opened by boot).
func TestRecoverInterruptedBatches_TransitionsPendingAndRunning(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	seedRows := map[string]uuid.UUID{
		"pending":     uuid.Must(uuid.NewRandom()),
		"running":     uuid.Must(uuid.NewRandom()),
		"completed":   uuid.Must(uuid.NewRandom()),
		"failed":      uuid.Must(uuid.NewRandom()),
		"cancelled":   uuid.Must(uuid.NewRandom()),
		"interrupted": uuid.Must(uuid.NewRandom()),
	}
	now := time.Now().UnixNano()
	for status, id := range seedRows {
		if err := s.InsertUpscaleBatch(UpscaleBatchRow{
			ID:         id,
			Path:       "scope/" + status,
			TargetRate: 192000,
			TargetBits: 24,
			Status:     status,
			CreatedAt:  now,
			UpdatedAt:  now,
		}); err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
	}

	recoverAt := now + 1_000_000 // 1 ms later, deterministic
	rowsAffected, err := s.RecoverInterruptedBatches(recoverAt)
	if err != nil {
		t.Fatalf("RecoverInterruptedBatches: %v", err)
	}
	if rowsAffected != 2 {
		t.Errorf("rowsAffected = %d, want 2 (pending + running)", rowsAffected)
	}

	// Read every row back; assert the transition.
	for status, id := range seedRows {
		var gotStatus string
		var gotUpdated int64
		err := s.db.QueryRow(
			`SELECT status, updated_at FROM upscale_batches WHERE id = ?`, id[:],
		).Scan(&gotStatus, &gotUpdated)
		if err != nil {
			t.Fatalf("read back %s: %v", status, err)
		}
		switch status {
		case "pending", "running":
			if gotStatus != "interrupted" {
				t.Errorf("row originally %q: got status %q, want interrupted",
					status, gotStatus)
			}
			if gotUpdated != recoverAt {
				t.Errorf("row originally %q: updated_at = %d, want %d",
					status, gotUpdated, recoverAt)
			}
		default:
			if gotStatus != status {
				t.Errorf("row originally %q: got status %q, want unchanged",
					status, gotStatus)
			}
			if gotUpdated != now {
				t.Errorf("row originally %q: updated_at = %d, want unchanged (%d)",
					status, gotUpdated, now)
			}
		}
	}
}

// TestRecoverInterruptedBatches_Idempotent locks the contract that
// repeated calls after the first don't mutate anything (rowsAffected
// = 0). Idempotency matters for bridge restart loops where boot may
// call the helper multiple times across a fast crash cycle.
func TestRecoverInterruptedBatches_Idempotent(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	id := uuid.Must(uuid.NewRandom())
	now := time.Now().UnixNano()
	if err := s.InsertUpscaleBatch(UpscaleBatchRow{
		ID: id, Path: "x", TargetRate: 192000, TargetBits: 24,
		Status: "running", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RecoverInterruptedBatches(now + 1); err != nil {
		t.Fatal(err)
	}
	// Second call: nothing to do.
	rows, err := s.RecoverInterruptedBatches(now + 2)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("second RecoverInterruptedBatches: rowsAffected = %d, want 0", rows)
	}
}

// TestSetGetUpscaleTarget_RoundTrip exercises the scan_state-backed
// settings pair.
func TestSetGetUpscaleTarget_RoundTrip(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	if err := s.SetUpscaleTarget(192000, 24); err != nil {
		t.Fatalf("SetUpscaleTarget: %v", err)
	}
	rate, bits, err := s.GetUpscaleTarget()
	if err != nil {
		t.Fatalf("GetUpscaleTarget: %v", err)
	}
	if rate != 192000 || bits != 24 {
		t.Errorf("got (rate=%d, bits=%d), want (192000, 24)", rate, bits)
	}

	// Overwrite — both keys flip atomically.
	if err := s.SetUpscaleTarget(96000, 16); err != nil {
		t.Fatalf("SetUpscaleTarget (overwrite): %v", err)
	}
	rate, bits, err = s.GetUpscaleTarget()
	if err != nil {
		t.Fatal(err)
	}
	if rate != 96000 || bits != 16 {
		t.Errorf("after overwrite: got (rate=%d, bits=%d), want (96000, 16)", rate, bits)
	}
}

// TestGetUpscaleTarget_UnsetReturnsSentinel locks the
// ErrUpscaleTargetUnset contract — callers (the coordinator) rely on
// this to fall back to bootstrap defaults rather than treating the
// missing rows as a 0/0 target.
func TestGetUpscaleTarget_UnsetReturnsSentinel(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	_, _, err := s.GetUpscaleTarget()
	if !errors.Is(err, ErrUpscaleTargetUnset) {
		t.Errorf("err = %v, want errors.Is(ErrUpscaleTargetUnset)", err)
	}
}

// TestSetUpscaleTarget_RejectsInvalidValues guards against malformed
// admin Settings input reaching the DB. Both rate and bits validation
// must run BEFORE the write commits — a partial-failure that lands
// one key without the other would leave the pair desynchronised.
func TestSetUpscaleTarget_RejectsInvalidValues(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	cases := []struct {
		name string
		rate int
		bits int
	}{
		{"zero rate", 0, 24},
		{"negative rate", -1, 24},
		{"invalid bits 8", 192000, 8},
		{"invalid bits 0", 192000, 0},
		{"invalid bits 17", 192000, 17},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := s.SetUpscaleTarget(c.rate, c.bits); err == nil {
				t.Errorf("SetUpscaleTarget(%d, %d): want error, got nil",
					c.rate, c.bits)
			}
			// Verify nothing landed in the DB on rejection — both
			// keys remain unset.
			if _, _, err := s.GetUpscaleTarget(); !errors.Is(err, ErrUpscaleTargetUnset) {
				t.Errorf("rejected write leaked partial state: %v", err)
			}
		})
	}
}
