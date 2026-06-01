package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newDeviceTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUpsertDeviceRegistrationInsertThenGet(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	if err := s.UpsertDeviceRegistration(ctx, "dev1", "tok-a", "Alice iPhone"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetDeviceByToken(ctx, "dev1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected registration, got nil")
	}
	if got.TokenID != "tok-a" || got.DeviceName != "Alice iPhone" {
		t.Errorf("got %+v, want tokenID=tok-a name=\"Alice iPhone\"", got)
	}
	if got.FirstSeenAt.IsZero() || got.LastSeenAt.IsZero() {
		t.Errorf("timestamps not set: %+v", got)
	}
}

func TestGetDeviceByTokenMissingReturnsNil(t *testing.T) {
	s := newDeviceTestStore(t)
	got, err := s.GetDeviceByToken(context.Background(), "nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for unknown token, got %+v", got)
	}
}

// TestUpsertDeviceRegistrationRebindKeepsFirstSeen verifies the ON CONFLICT
// path is the rebind: a new auth token for the same device token updates
// token_id + last_seen but preserves first_seen_at.
func TestUpsertDeviceRegistrationRebindKeepsFirstSeen(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()

	// Inject a stepping clock so first_seen vs last_seen are distinguishable
	// without wall-clock sleeps.
	base := time.Unix(1_700_000_000, 0)
	var step int64
	s.now = func() time.Time { step++; return base.Add(time.Duration(step) * time.Second) }

	if err := s.UpsertDeviceRegistration(ctx, "dev1", "tok-a", "Alice iPhone"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first, _ := s.GetDeviceByToken(ctx, "dev1")

	// Re-pair: same device token, new auth token, empty name.
	if err := s.UpsertDeviceRegistration(ctx, "dev1", "tok-b", ""); err != nil {
		t.Fatalf("rebind upsert: %v", err)
	}
	after, _ := s.GetDeviceByToken(ctx, "dev1")

	if after.TokenID != "tok-b" {
		t.Errorf("token_id not rebound: got %q want tok-b", after.TokenID)
	}
	if !after.FirstSeenAt.Equal(first.FirstSeenAt) {
		t.Errorf("first_seen_at changed across rebind: %v -> %v", first.FirstSeenAt, after.FirstSeenAt)
	}
	if !after.LastSeenAt.After(first.LastSeenAt) {
		t.Errorf("last_seen_at did not advance: %v -> %v", first.LastSeenAt, after.LastSeenAt)
	}
	// Empty name on rebind must NOT clobber the captured name.
	if after.DeviceName != "Alice iPhone" {
		t.Errorf("empty-name rebind clobbered name: got %q", after.DeviceName)
	}
}

// TestUpsertDeviceRegistrationNonEmptyNameOverwrites verifies a later
// non-empty name (e.g. captured at pairing approval after a header-path
// bootstrap) does overwrite a prior name.
func TestUpsertDeviceRegistrationNonEmptyNameOverwrites(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	if err := s.UpsertDeviceRegistration(ctx, "dev1", "tok-a", ""); err != nil {
		t.Fatalf("header-path upsert: %v", err)
	}
	if err := s.UpsertDeviceRegistration(ctx, "dev1", "tok-a", "Bob iPad"); err != nil {
		t.Fatalf("approve-path upsert: %v", err)
	}
	got, _ := s.GetDeviceByToken(ctx, "dev1")
	if got.DeviceName != "Bob iPad" {
		t.Errorf("non-empty name did not overwrite: got %q", got.DeviceName)
	}
}

func TestUpsertDeviceRegistrationRejectsEmptyToken(t *testing.T) {
	s := newDeviceTestStore(t)
	if err := s.UpsertDeviceRegistration(context.Background(), "", "tok", "n"); err == nil {
		t.Fatal("expected error for empty device token, got nil")
	}
}

func TestListDeviceRegistrationsOrderedByLastSeenDesc(t *testing.T) {
	s := newDeviceTestStore(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	var step int64
	s.now = func() time.Time { step++; return base.Add(time.Duration(step) * time.Second) }

	_ = s.UpsertDeviceRegistration(ctx, "devA", "tA", "A")
	_ = s.UpsertDeviceRegistration(ctx, "devB", "tB", "B")
	_ = s.UpsertDeviceRegistration(ctx, "devA", "tA", "A") // touch devA again → newest

	list, err := s.ListDeviceRegistrations(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 registrations, got %d", len(list))
	}
	if list[0].DeviceToken != "devA" {
		t.Errorf("expected devA first (most recent last_seen), got %q", list[0].DeviceToken)
	}
}
