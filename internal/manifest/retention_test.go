package manifest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func retentionStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func countRows(t *testing.T, s *Store, table string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestReapOrphanDeviceRegistrationsFailsClosedOnAnEmptySet is THE
// assertion of this file. An empty live-token set means "every
// registration is orphaned", and the realistic way to arrive at one is a
// failure to read the auth store — not a genuinely token-less bridge.
// Deleting everything on a read failure is the one outcome this must
// never produce.
func TestReapOrphanDeviceRegistrationsFailsClosedOnAnEmptySet(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.UpsertDeviceRegistration(ctx, fmt.Sprintf("dev%d", i), "tok-live", "Phone"); err != nil {
			t.Fatal(err)
		}
	}
	before := countRows(t, s, "device_registrations")
	if before != 3 {
		t.Fatalf("fixture: want 3 registrations, got %d", before)
	}

	n, err := s.ReapOrphanDeviceRegistrations(ctx, nil)
	if !errors.Is(err, ErrNoLiveTokens) {
		t.Fatalf("want ErrNoLiveTokens for a nil set, got %v", err)
	}
	if n != 0 {
		t.Errorf("reported %d rows deleted on a refused call", n)
	}
	if got := countRows(t, s, "device_registrations"); got != before {
		t.Fatalf("an empty live-token set deleted %d of %d registrations; it MUST delete none",
			before-got, before)
	}

	// The EMPTY SLICE is the dangerous spelling, and the one cmd/bridge
	// actually produces (`make([]string, 0, n)` with nothing appended).
	// Measured without the guard: nil deletes 0 rows (json_each('null')
	// yields a NULL row, so `NOT IN (NULL)` is never true) while
	// []string{} deletes EVERY row (`NOT IN (<empty>)` is true). The two
	// empty forms are not interchangeable.
	if _, err := s.ReapOrphanDeviceRegistrations(ctx, []string{}); !errors.Is(err, ErrNoLiveTokens) {
		t.Fatalf("want ErrNoLiveTokens for an empty slice, got %v", err)
	}
	if got := countRows(t, s, "device_registrations"); got != before {
		t.Fatalf("an empty SLICE deleted %d of %d registrations — this is the spelling that "+
			"wipes the table without the guard", before-got, before)
	}
}

func TestReapOrphanDeviceRegistrationsDeletesOnlyRevokedBindings(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	if err := s.UpsertDeviceRegistration(ctx, "dev-live", "tok-live", "Phone"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDeviceRegistration(ctx, "dev-revoked", "tok-gone", "Old iPad"); err != nil {
		t.Fatal(err)
	}

	n, err := s.ReapOrphanDeviceRegistrations(ctx, []string{"tok-live", "tok-unused"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want exactly the one bound to a revoked token", n)
	}
	if d, _ := s.GetDeviceByToken(ctx, "dev-live"); d == nil {
		t.Error("the live registration was deleted")
	}
	if d, _ := s.GetDeviceByToken(ctx, "dev-revoked"); d != nil {
		t.Error("the orphaned registration survived")
	}
}

// TestReapingARegistrationLeavesItsHistoryReadable pins the interaction
// review flagged as a foreign-key blocker. There is no foreign key:
// history rows LEFT JOIN registrations and degrade to unattributed, which
// is a supported state.
func TestReapingARegistrationLeavesItsHistoryReadable(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	if err := s.UpsertDeviceRegistration(ctx, "dev-gone", "tok-gone", "Old iPad"); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertHistoryBatch(ctx, []PlaybackHistoryRow{{
		DeviceToken: "dev-gone", Path: "A/B/01.flac",
		StartedAt: time.Now().UnixNano(), DurationUsed: 120,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReapOrphanDeviceRegistrations(ctx, []string{"tok-other"}); err != nil {
		t.Fatalf("reap failed — a foreign key would surface here: %v", err)
	}
	if got := countRows(t, s, "playback_history"); got != 1 {
		t.Fatalf("history rows = %d after reaping their device; want them preserved", got)
	}
	rows, err := s.ListHistory(ctx, "", 10, 0)
	if err != nil {
		t.Fatalf("ListHistory after the reap: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListHistory returned %d rows, want 1 unattributed row", len(rows))
	}
}

func TestReapPlaybackHistoryHonoursTheCutoff(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	now := time.Now()
	mk := func(age time.Duration, path string) PlaybackHistoryRow {
		return PlaybackHistoryRow{
			DeviceToken: "dev", Path: path,
			StartedAt: now.Add(-age).UnixNano(), DurationUsed: 60,
		}
	}
	if err := s.InsertHistoryBatch(ctx, []PlaybackHistoryRow{
		mk(400*24*time.Hour, "old.flac"),
		mk(200*24*time.Hour, "middle.flac"),
		mk(10*24*time.Hour, "recent.flac"),
	}); err != nil {
		t.Fatal(err)
	}

	cutoff := now.AddDate(0, 0, -365).UnixNano()
	n, err := s.ReapPlaybackHistory(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want 1 (only the 400-day-old event)", n)
	}
	if got := countRows(t, s, "playback_history"); got != 2 {
		t.Errorf("history rows = %d, want 2", got)
	}

	// A non-positive cutoff is a no-op, never "delete everything".
	if n, err := s.ReapPlaybackHistory(ctx, 0); err != nil || n != 0 {
		t.Errorf("cutoff 0: n=%d err=%v; want a no-op", n, err)
	}
	if n, err := s.ReapPlaybackHistory(ctx, -1); err != nil || n != 0 {
		t.Errorf("negative cutoff: n=%d err=%v; want a no-op", n, err)
	}
	if got := countRows(t, s, "playback_history"); got != 2 {
		t.Errorf("a non-positive cutoff deleted rows; count now %d", got)
	}
}

func TestReapStaleDeviceRegistrationsHonoursTheCutoff(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now.Add(-400 * 24 * time.Hour) }
	if err := s.UpsertDeviceRegistration(ctx, "dev-old", "tok", "Drawer iPad"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	if err := s.UpsertDeviceRegistration(ctx, "dev-new", "tok", "Phone"); err != nil {
		t.Fatal(err)
	}

	n, err := s.ReapStaleDeviceRegistrations(ctx, now.AddDate(0, 0, -365).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want 1", n)
	}
	if d, _ := s.GetDeviceByToken(ctx, "dev-new"); d == nil {
		t.Error("the recently-seen registration was deleted")
	}

	if n, err := s.ReapStaleDeviceRegistrations(ctx, 0); err != nil || n != 0 {
		t.Errorf("cutoff 0: n=%d err=%v; want a no-op", n, err)
	}
}

func TestRetentionCountsReportsBothTables(t *testing.T) {
	s := retentionStore(t)
	ctx := context.Background()

	rc, err := s.RetentionCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rc.PlaybackHistoryRows != 0 || rc.DeviceRegistrationRows != 0 || rc.OldestPlaybackStartedAt != 0 {
		t.Fatalf("empty store reported %+v", rc)
	}

	if err := s.UpsertDeviceRegistration(ctx, "dev", "tok", "Phone"); err != nil {
		t.Fatal(err)
	}
	oldest := time.Now().Add(-72 * time.Hour).UnixNano()
	if err := s.InsertHistoryBatch(ctx, []PlaybackHistoryRow{
		{DeviceToken: "dev", Path: "a.flac", StartedAt: oldest, DurationUsed: 30},
		{DeviceToken: "dev", Path: "b.flac", StartedAt: time.Now().UnixNano(), DurationUsed: 30},
	}); err != nil {
		t.Fatal(err)
	}

	rc, err = s.RetentionCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rc.PlaybackHistoryRows != 2 || rc.DeviceRegistrationRows != 1 {
		t.Errorf("counts = %+v, want 2 history / 1 registration", rc)
	}
	if rc.OldestPlaybackStartedAt != oldest {
		t.Errorf("OldestPlaybackStartedAt = %d, want %d", rc.OldestPlaybackStartedAt, oldest)
	}
}
