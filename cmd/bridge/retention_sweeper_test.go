package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func retentionFixture(t *testing.T) *manifest.Store {
	t.Helper()
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedRegistrations(t *testing.T, s *manifest.Store, tokenIDs ...string) {
	t.Helper()
	ctx := context.Background()
	for i, tok := range tokenIDs {
		if err := s.UpsertDeviceRegistration(ctx, fmt.Sprintf("dev%d", i), tok, "Phone"); err != nil {
			t.Fatal(err)
		}
	}
}

func regCount(t *testing.T, s *manifest.Store) int64 {
	t.Helper()
	rc, err := s.RetentionCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return rc.DeviceRegistrationRows
}

// countingReaper wraps a real store and records whether each reap was
// CALLED. Observing the call is the only way to pin the sweeper's
// fail-closed skip: the store carries its own ErrNoLiveTokens guard, so
// registrations survive a read failure either way and a rows-survived
// assertion goes green against a sweeper that skips nothing. (Verified —
// the first version of this test did exactly that.)
type countingReaper struct {
	inner       retentionReaper
	orphanCalls int
	orphanArgs  [][]string
}

func (c *countingReaper) ReapOrphanDeviceRegistrations(ctx context.Context, ids []string) (int64, error) {
	c.orphanCalls++
	c.orphanArgs = append(c.orphanArgs, ids)
	return c.inner.ReapOrphanDeviceRegistrations(ctx, ids)
}

func (c *countingReaper) ReapStaleDeviceRegistrations(ctx context.Context, beforeNS int64) (int64, error) {
	return c.inner.ReapStaleDeviceRegistrations(ctx, beforeNS)
}

func (c *countingReaper) ReapPlaybackHistory(ctx context.Context, beforeNS int64) (int64, error) {
	return c.inner.ReapPlaybackHistory(ctx, beforeNS)
}

// TestSweepSkipsTheOrphanReapWhenTheTokenSetCannotBeRead is the assertion
// that matters. The realistic way to arrive at an empty live-token set is
// a failure to READ the auth store, and reaping against one would delete
// every registration on the bridge.
//
// It asserts the reap is never ATTEMPTED, not merely that rows survived —
// see countingReaper for why the weaker form proves nothing.
func TestSweepSkipsTheOrphanReapWhenTheTokenSetCannotBeRead(t *testing.T) {
	s := retentionFixture(t)
	seedRegistrations(t, s, "tok-a", "tok-b")
	before := regCount(t, s)
	c := &countingReaper{inner: s}

	r := &retentionSweeper{
		store:     c,
		liveToken: func() ([]string, error) { return nil, errors.New("tokens.json unreadable") },
		cfg:       func() *config.Config { return &config.Config{} },
		now:       time.Now,
	}
	r.sweep(context.Background())

	if c.orphanCalls != 0 {
		t.Errorf("the orphan reap was attempted %d time(s) after a token-read failure, with %v; "+
			"the sweep must not ask at all", c.orphanCalls, c.orphanArgs)
	}
	if got := regCount(t, s); got != before {
		t.Fatalf("a token-read failure deleted %d of %d registrations", before-got, before)
	}
}

// A bridge with no tokens minted yet is a normal pre-pairing state, not an
// error — and it must also never reach the reap.
func TestSweepSkipsTheOrphanReapOnAnEmptyTokenSet(t *testing.T) {
	s := retentionFixture(t)
	seedRegistrations(t, s, "tok-a")
	before := regCount(t, s)

	for _, empty := range [][]string{nil, {}} {
		c := &countingReaper{inner: s}
		r := &retentionSweeper{
			store:     c,
			liveToken: func() ([]string, error) { return empty, nil },
			cfg:       func() *config.Config { return &config.Config{} },
			now:       time.Now,
		}
		r.sweep(context.Background())
		if c.orphanCalls != 0 {
			t.Errorf("the orphan reap was attempted with an empty set (%v)", empty)
		}
		if got := regCount(t, s); got != before {
			t.Fatalf("an empty token set (%v) deleted registrations", empty)
		}
	}
}

func TestSweepReapsOnlyRevokedBindings(t *testing.T) {
	s := retentionFixture(t)
	seedRegistrations(t, s, "tok-live", "tok-revoked")

	r := &retentionSweeper{
		store:     s,
		liveToken: func() ([]string, error) { return []string{"tok-live"}, nil },
		cfg:       func() *config.Config { return &config.Config{} },
		now:       time.Now,
	}
	r.sweep(context.Background())

	if got := regCount(t, s); got != 1 {
		t.Errorf("registrations = %d, want 1 (the live binding)", got)
	}
}

// TestSweepHonoursTheWindowsAndDefaultsToOff pins that a zero-valued
// config is a no-op for both time windows — the shipped default must not
// delete anything.
func TestSweepHonoursTheWindowsAndDefaultsToOff(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	seedHistory := func(s *manifest.Store) {
		if err := s.InsertHistoryBatch(ctx, []manifest.PlaybackHistoryRow{
			{DeviceToken: "dev0", Path: "old.flac",
				StartedAt: now.Add(-400 * 24 * time.Hour).UnixNano(), DurationUsed: 60},
			{DeviceToken: "dev0", Path: "new.flac",
				StartedAt: now.UnixNano(), DurationUsed: 60},
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("default config deletes nothing", func(t *testing.T) {
		s := retentionFixture(t)
		seedRegistrations(t, s, "tok-live")
		seedHistory(s)
		r := &retentionSweeper{
			store:     s,
			liveToken: func() ([]string, error) { return []string{"tok-live"}, nil },
			cfg:       func() *config.Config { return &config.Config{} },
			now:       func() time.Time { return now },
		}
		r.sweep(ctx)
		rc, _ := s.RetentionCounts(ctx)
		if rc.PlaybackHistoryRows != 2 {
			t.Errorf("history rows = %d, want 2 — the default must keep everything", rc.PlaybackHistoryRows)
		}
		if rc.DeviceRegistrationRows != 1 {
			t.Errorf("registrations = %d, want 1", rc.DeviceRegistrationRows)
		}
	})

	t.Run("configured window reaps past it", func(t *testing.T) {
		s := retentionFixture(t)
		seedRegistrations(t, s, "tok-live")
		seedHistory(s)
		r := &retentionSweeper{
			store:     s,
			liveToken: func() ([]string, error) { return []string{"tok-live"}, nil },
			cfg: func() *config.Config {
				return &config.Config{Retention: config.RetentionConfig{PlaybackHistoryDays: 365}}
			},
			now: func() time.Time { return now },
		}
		r.sweep(ctx)
		rc, _ := s.RetentionCounts(ctx)
		if rc.PlaybackHistoryRows != 1 {
			t.Errorf("history rows = %d, want 1 (the 400-day-old event reaped)", rc.PlaybackHistoryRows)
		}
	})
}

// TestSweepReadsConfigLive pins the hot-apply contract: a settings change
// takes effect on the next tick, not at the next restart.
func TestSweepReadsConfigLive(t *testing.T) {
	s := retentionFixture(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertHistoryBatch(ctx, []manifest.PlaybackHistoryRow{
		{DeviceToken: "dev", Path: "old.flac",
			StartedAt: now.Add(-400 * 24 * time.Hour).UnixNano(), DurationUsed: 60},
	}); err != nil {
		t.Fatal(err)
	}

	live := &config.Config{}
	r := &retentionSweeper{
		store:     s,
		liveToken: func() ([]string, error) { return []string{"tok"}, nil },
		cfg:       func() *config.Config { return live },
		now:       func() time.Time { return now },
	}

	r.sweep(ctx)
	if rc, _ := s.RetentionCounts(ctx); rc.PlaybackHistoryRows != 1 {
		t.Fatalf("precondition: the row should survive an off config; got %d", rc.PlaybackHistoryRows)
	}

	// Change the config WITHOUT rebuilding the sweeper.
	live.Retention.PlaybackHistoryDays = 90
	r.sweep(ctx)
	if rc, _ := s.RetentionCounts(ctx); rc.PlaybackHistoryRows != 0 {
		t.Errorf("history rows = %d after enabling retention; the sweep must read the config live",
			rc.PlaybackHistoryRows)
	}
}
