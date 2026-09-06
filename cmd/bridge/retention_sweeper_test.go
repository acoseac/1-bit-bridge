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
//
// It records the two WINDOW cutoffs for the same reason one level along:
// from cmd/bridge the store's clock is unreachable (`Store.now` is
// unexported), so a behavioural "was a stale row deleted" assertion needs
// a fixture this package cannot build. What CAN be pinned from here — and
// is the thing that was missing — is that the config field reaches the
// reap at all, with the cutoff it implies.
type countingReaper struct {
	inner        retentionReaper
	orphanCalls  int
	orphanArgs   [][]string
	staleCalls   int
	staleCutoffs []int64
	histCalls    int
	histCutoffs  []int64
}

func (c *countingReaper) ReapOrphanDeviceRegistrations(ctx context.Context, ids []string) (int64, error) {
	c.orphanCalls++
	c.orphanArgs = append(c.orphanArgs, ids)
	return c.inner.ReapOrphanDeviceRegistrations(ctx, ids)
}

func (c *countingReaper) ReapStaleDeviceRegistrations(ctx context.Context, beforeNS int64) (int64, error) {
	c.staleCalls++
	c.staleCutoffs = append(c.staleCutoffs, beforeNS)
	return c.inner.ReapStaleDeviceRegistrations(ctx, beforeNS)
}

func (c *countingReaper) ReapPlaybackHistory(ctx context.Context, beforeNS int64) (int64, error) {
	c.histCalls++
	c.histCutoffs = append(c.histCutoffs, beforeNS)
	return c.inner.ReapPlaybackHistory(ctx, beforeNS)
}

// TestSweepSkipsTheOrphanReapWhenTheTokenSetCannotBeRead is the assertion
// that matters. The realistic way to arrive at an empty live-token set is
// a failure to READ the auth store, and reaping against one would delete
// every registration on the bridge.
//
// It asserts the reap is never ATTEMPTED, not merely that rows survived —
// see countingReaper for why the weaker form proves nothing.
//
// The FIXTURE is load-bearing too, and this is the second time that has
// had to be learned here. `(nil, error)` cannot pin the fail-closed skip,
// because a nil slice also satisfies the `len(ids) == 0` branch beside
// it: deleting the `err != nil` case entirely leaves this test green.
// Only a PARTIAL read — some tokens AND an error — separates the two, so
// that is what this hands the sweeper. (CLAUDE.md: "a fixture must be a
// value the transformation would actually change.")
func TestSweepSkipsTheOrphanReapWhenTheTokenSetCannotBeRead(t *testing.T) {
	s := retentionFixture(t)
	seedRegistrations(t, s, "tok-a", "tok-b")
	before := regCount(t, s)
	c := &countingReaper{inner: s}

	r := &retentionSweeper{
		store: c,
		liveToken: func() ([]string, error) {
			return []string{"tok-a"}, errors.New("tokens.json truncated mid-read")
		},
		cfg: func() *config.Config { return &config.Config{} },
		now: time.Now,
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

	// The registration window had NO sweeper-level coverage at all:
	// deleting the whole branch left the entire cmd/bridge suite green.
	// Only the store method was tested, and nothing pinned that the config
	// field reaches it — which matters more here than for its sibling,
	// because ErrNoLiveTokens guards only the ORPHAN reap, so this is the
	// branch with no second line of defence.
	t.Run("the registration window reaches its reap, with the right cutoff", func(t *testing.T) {
		s := retentionFixture(t)
		seedRegistrations(t, s, "tok-live")
		c := &countingReaper{inner: s}
		r := &retentionSweeper{
			store:     c,
			liveToken: func() ([]string, error) { return []string{"tok-live"}, nil },
			cfg: func() *config.Config {
				return &config.Config{Retention: config.RetentionConfig{DeviceRegistrationDays: 365}}
			},
			now: func() time.Time { return now },
		}
		r.sweep(ctx)
		if c.staleCalls != 1 {
			t.Fatalf("the stale-registration reap was called %d time(s), want 1 — the config "+
				"field must reach it", c.staleCalls)
		}
		if want := now.AddDate(0, 0, -365).UnixNano(); c.staleCutoffs[0] != want {
			t.Errorf("stale cutoff = %d, want %d (365 days back from the sweeper's clock)",
				c.staleCutoffs[0], want)
		}
		// And it stays off by default, on the same fixture.
		c2 := &countingReaper{inner: s}
		r.store = c2
		r.cfg = func() *config.Config { return &config.Config{} }
		r.sweep(ctx)
		if c2.staleCalls != 0 {
			t.Errorf("the stale-registration reap ran %d time(s) with the window off", c2.staleCalls)
		}
	})
}

// TestSweepRefusesAnOverflowedWindowRatherThanEmptyingTheTable drives the
// REAL sweeper against the REAL store with the day count that used to
// wipe both tables.
//
// config.MaxRetentionDays now refuses 999999 at load, so this value can
// no longer arrive from a validated config — but the sweeper takes a
// *config.Config, and a struct literal is exactly how it arrives here and
// in five other tests. This pins the store-level belt end to end: the
// pass must reap NOTHING and leave both tables intact.
//
// Before the belt, this same fixture emptied both: `reaped playback
// history past the window rows=2 days=999999`, logged as a success.
func TestSweepRefusesAnOverflowedWindowRatherThanEmptyingTheTable(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := retentionFixture(t)
	seedRegistrations(t, s, "tok-live")
	if err := s.InsertHistoryBatch(ctx, []manifest.PlaybackHistoryRow{
		{DeviceToken: "dev0", Path: "old.flac",
			StartedAt: now.Add(-400 * 24 * time.Hour).UnixNano(), DurationUsed: 60},
		{DeviceToken: "dev0", Path: "new.flac", StartedAt: now.UnixNano(), DurationUsed: 60},
	}); err != nil {
		t.Fatal(err)
	}

	// Precondition: this day count really does overflow into the future.
	// Without it the test could pass because the arithmetic changed, not
	// because the guard worked.
	if cutoff := now.AddDate(0, 0, -999999).UnixNano(); cutoff < now.UnixNano() {
		t.Fatalf("fixture: days=999999 no longer overflows into the future (cutoff=%d, now=%d); "+
			"this test would prove nothing — pick a day count that does",
			cutoff, now.UnixNano())
	}

	r := &retentionSweeper{
		store:     s,
		liveToken: func() ([]string, error) { return []string{"tok-live"}, nil },
		cfg: func() *config.Config {
			return &config.Config{Retention: config.RetentionConfig{
				PlaybackHistoryDays:    999999,
				DeviceRegistrationDays: 999999,
			}}
		},
		now: func() time.Time { return now },
	}
	r.sweep(ctx)

	rc, err := s.RetentionCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rc.PlaybackHistoryRows != 2 {
		t.Errorf("history rows = %d, want 2 — an overflowed window emptied the table", rc.PlaybackHistoryRows)
	}
	if rc.DeviceRegistrationRows != 1 {
		t.Errorf("registrations = %d, want 1 — an overflowed window emptied the table",
			rc.DeviceRegistrationRows)
	}
}

// TestSweepDoesNothingOnACancelledContext pins that a shutdown landing
// inside a pass is a clean exit rather than three Warn lines about a
// failure that is not one.
func TestSweepDoesNothingOnACancelledContext(t *testing.T) {
	s := retentionFixture(t)
	seedRegistrations(t, s, "tok-a")
	c := &countingReaper{inner: s}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &retentionSweeper{
		store:     c,
		liveToken: func() ([]string, error) { return []string{"tok-live"}, nil },
		cfg: func() *config.Config {
			return &config.Config{Retention: config.RetentionConfig{
				PlaybackHistoryDays: 90, DeviceRegistrationDays: 90,
			}}
		},
		now: time.Now,
	}
	r.sweep(ctx)
	if c.orphanCalls+c.staleCalls+c.histCalls != 0 {
		t.Errorf("a cancelled context still reached the reaps: orphan=%d stale=%d history=%d",
			c.orphanCalls, c.staleCalls, c.histCalls)
	}
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
