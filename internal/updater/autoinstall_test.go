//go:build !windows

package updater

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// makeAutoInstallUpdater stands up an Updater pre-loaded with a
// known status + an install path that fails fast (no real archive
// download — we only want to exercise the gate logic). The
// in-memory status simulates "GitHub has 0.2.0; we're on 0.1.0".
func makeAutoInstallUpdater(t *testing.T, opts Options) (*Updater, *atomic.Int32) {
	t.Helper()
	restartCalls := &atomic.Int32{}
	if opts.AutoInstallRestart == nil {
		opts.AutoInstallRestart = func() { restartCalls.Add(1) }
	}
	if opts.AutoInstallOpts == nil {
		opts.AutoInstallOpts = &InstallOptions{
			DataDir:    t.TempDir(),
			BinaryPath: "/nonexistent/bridge", // Install will fail at preflight; gate runs before
			Sessions:   NewTracker(),
			Force:      true,
			Verifier:   noopVerifier,
		}
	}
	upd := New(opts)
	upd.mu.Lock()
	upd.status.LatestVersion = "9.9.9"
	upd.status.UpdateAvailable = true
	upd.status.CurrentVersion = "0.0.1"
	upd.mu.Unlock()
	return upd, restartCalls
}

func TestMaybeAutoInstallSkipsWhenDisabled(t *testing.T) {
	upd, restarts := makeAutoInstallUpdater(t, Options{AutoInstall: false})
	upd.maybeAutoInstall(context.Background())
	if restarts.Load() != 0 {
		t.Errorf("restart fired %d times with AutoInstall=false (want 0)", restarts.Load())
	}
}

func TestMaybeAutoInstallSkipsOutsideQuietHours(t *testing.T) {
	// Window 02:00-04:00; clock pinned at 12:00. Auto-install should defer.
	now := func() time.Time {
		return time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	}
	upd, restarts := makeAutoInstallUpdater(t, Options{
		AutoInstall:     true,
		QuietHoursStart: 120, // 02:00
		QuietHoursEnd:   240, // 04:00
		Now:             now,
	})
	upd.maybeAutoInstall(context.Background())
	if restarts.Load() != 0 {
		t.Errorf("restart fired %d times outside quiet-hours window", restarts.Load())
	}
}

func TestMaybeAutoInstallSkipsWithActiveSessions(t *testing.T) {
	tracker := NewTracker()
	tracker.Begin()
	defer tracker.End()

	upd, restarts := makeAutoInstallUpdater(t, Options{
		AutoInstall: true,
		AutoInstallOpts: &InstallOptions{
			DataDir:    t.TempDir(),
			BinaryPath: "/nonexistent/bridge",
			Sessions:   tracker,
			Force:      false,
			Verifier:   noopVerifier,
		},
	})
	upd.maybeAutoInstall(context.Background())
	if restarts.Load() != 0 {
		t.Errorf("restart fired %d times with active session (want 0)", restarts.Load())
	}
}

func TestMaybeAutoInstallSkipsWhenNoUpdate(t *testing.T) {
	upd, restarts := makeAutoInstallUpdater(t, Options{AutoInstall: true})
	// Override the pre-loaded status: nothing to install.
	upd.mu.Lock()
	upd.status.UpdateAvailable = false
	upd.status.LatestVersion = ""
	upd.mu.Unlock()

	upd.maybeAutoInstall(context.Background())
	if restarts.Load() != 0 {
		t.Errorf("restart fired %d times with no update available", restarts.Load())
	}
}

func TestInWindow(t *testing.T) {
	// Mirror config.IsInQuietHours; the two implementations stay in
	// lockstep so a future refactor can collapse them.
	cases := []struct {
		start, end, now int
		want            bool
	}{
		{0, 360, 120, true},
		{0, 360, 720, false},
		{1380, 360, 60, true},   // wrap-around
		{1380, 360, 720, false}, // wrap-around outside
		{500, 500, 500, false},  // degenerate
	}
	for _, c := range cases {
		if got := inWindow(c.start, c.end, c.now); got != c.want {
			t.Errorf("inWindow(%d,%d,%d) = %v, want %v",
				c.start, c.end, c.now, got, c.want)
		}
	}
}

func TestNewClampsCheckIntervalAcceptsZero(t *testing.T) {
	// A zero CheckInterval picks the default; values below
	// minCheckInterval clamp up. Both branches matter for the
	// auto-installer because the poll cadence drives the install
	// cadence.
	u := New(Options{CheckInterval: 0})
	if u.interval != DefaultCheckInterval {
		t.Errorf("interval default: got %v, want %v", u.interval, DefaultCheckInterval)
	}
	u2 := New(Options{CheckInterval: time.Second})
	if u2.interval != minCheckInterval {
		t.Errorf("interval clamp: got %v, want %v", u2.interval, minCheckInterval)
	}
}
