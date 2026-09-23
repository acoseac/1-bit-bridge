//go:build !windows

package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// bakBody reads the rollback target's contents, or "" when there is none.
func bakBody(t *testing.T, livePath string) string {
	t.Helper()
	b, err := os.ReadFile(livePath + ".bak")
	if err != nil {
		return ""
	}
	return string(b)
}

// retryOptsFor rebuilds the InstallOptions the fixture used, so a second
// attempt is byte-identical to the first in everything except the fact
// that a swap has already happened.
func retryOptsFor(livePath string) InstallOptions {
	return InstallOptions{
		DataDir:    filepath.Dir(livePath),
		BinaryPath: livePath,
		Force:      true,
		Verifier:   noopVerifier,
	}
}

// TestSecondInstallWithoutARestartKeepsTheRollbackTarget is the defect.
//
// apiUpdatesInstall does not restart — restart is a separate operator
// action — and u.status.CurrentVersion is written once at construction,
// so UpdateAvailable stayed true and the console kept offering Install.
// The second install then re-ran the whole download and reached
// swapBinary, whose EEXIST retry does os.Remove(bak) before re-linking:
// .bak went from holding 0.1.0 (the operator's real rollback target) to
// holding 0.2.0, a copy of what was already live. canRollback() reported
// true throughout, because it only stats for the file's existence.
func TestSecondInstallWithoutARestartKeepsTheRollbackTarget(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	livePath, upd, err := fix.install(t, "0.1.0")
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if got, want := bakBody(t, livePath), "bridge-binary-0.1.0"; got != want {
		t.Fatalf("after the first install .bak = %q, want %q", got, want)
	}

	_, err = upd.Install(context.Background(), retryOptsFor(livePath))
	// Errorf, not Fatalf: the .bak assertion below is the one that
	// describes the DAMAGE, and a Fatalf here would stop the test before
	// reaching it — so a negative control would prove only that the
	// error changed, never that the rollback target was actually being
	// destroyed. Assert on the files (#941's lesson).
	if !errors.Is(err, ErrInstallPendingRestart) {
		t.Errorf("second Install: err = %v, want ErrInstallPendingRestart", err)
	}
	if got, want := bakBody(t, livePath), "bridge-binary-0.1.0"; got != want {
		t.Errorf("the rollback target was destroyed by the second install: .bak = %q, want %q", got, want)
	}
}

// TestSecondInstallIsRefusedFromAFreshUpdater is the half the in-memory
// flag cannot cover.
//
// `bridge update` is a SEPARATE PROCESS: it constructs its own Updater,
// so an atomic.Bool the serving bridge set is invisible to it. The
// refusal has to come off the PERSISTED marker, which is why
// swapAwaitingRestart reads update-state.json rather than trusting
// pendingRestart alone.
func TestSecondInstallIsRefusedFromAFreshUpdater(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	livePath, _, err := fix.install(t, "0.1.0")
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}

	// A second process: brand-new Updater, same data dir, nothing in
	// memory that knows an install happened.
	fresh := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL),
	})
	fresh.mu.Lock()
	fresh.status.CurrentVersion = "0.1.0"
	fresh.status.LatestVersion = fix.latestVersion
	fresh.status.UpdateAvailable = true
	fresh.mu.Unlock()
	if fresh.pendingRestart.Load() {
		t.Fatal("fixture error: a fresh Updater must not start with pendingRestart set")
	}

	_, err = fresh.Install(context.Background(), retryOptsFor(livePath))
	if !errors.Is(err, ErrInstallPendingRestart) {
		t.Errorf("fresh-process Install: err = %v, want ErrInstallPendingRestart", err)
	}
	if got, want := bakBody(t, livePath), "bridge-binary-0.1.0"; got != want {
		t.Errorf("the rollback target was destroyed by a second process: .bak = %q, want %q", got, want)
	}
}

// TestANewerReleaseIsStillInstallable is the positive control, and it is
// the one that stops the guard from being "refuse every second install".
//
// Refusing a NEWER target would strand the host on a version it has not
// even booted, which is worse than the bug. Only a repeat of the target
// already on disk is the defect, so swapAwaitingRestart compares the
// version and not merely the presence of a marker.
func TestANewerReleaseIsStillInstallable(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	livePath, upd, err := fix.install(t, "0.1.0")
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}

	// A newer release appears. Point the Updater at a fixture serving
	// it, exactly as a fresh poll would.
	newer := newInstallFixture(t, "0.3.0")
	upd.client = NewClient("fake/repo", time.Second).WithBaseURL(newer.server.URL)
	upd.mu.Lock()
	upd.status.LatestVersion = "0.3.0"
	upd.status.UpdateAvailable = true
	upd.mu.Unlock()

	if _, err := upd.Install(context.Background(), retryOptsFor(livePath)); err != nil {
		t.Fatalf("installing a NEWER release was refused: %v", err)
	}
	live, rerr := os.ReadFile(livePath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got, want := string(live), "bridge-binary-0.3.0"; got != want {
		t.Errorf("live binary = %q, want %q", got, want)
	}
}

// TestAManualInstallDoesNotArmTheAutoRestart is CodeRabbit's Major on
// #977, and the contract was written on the field itself: pendingRestart's
// docblock says "a manual admin/CLI install must NOT trigger the
// auto-installer's restart".
//
// An earlier draft of the repeat-install guard set pendingRestart from
// Install for every path. With autoInstall enabled, a later eligible poll
// passes that flag to restartWhenDrained — so an operator who deliberately
// installed WITHOUT restarting would have been restarted anyway, hours
// later. The guard needs the STATE (a swap landed); the restart is an
// INTENT, and only maybeAutoInstall owns it.
func TestAManualInstallDoesNotArmTheAutoRestart(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	livePath, upd, err := fix.install(t, "0.1.0")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if upd.pendingRestart.Load() {
		t.Error("a manual install armed the auto-installer's restart intent")
	}
	if !upd.swapPending.Load() {
		t.Error("a manual install did not record that a swap landed")
	}
	// And the guard still works off the state, which is the whole point
	// of keeping them apart.
	if !upd.swapAwaitingRestart(filepath.Dir(livePath), "0.2.0") {
		t.Error("the repeat-install guard lost its signal when the flags were split")
	}
}

// TestTheGuardSurvivesAnExpiredRecencyWindow is CodeRabbit's Minor on
// #977.
//
// recencyWindow is six hours and a manual restart can take longer. Past
// it the persisted marker reads abandoned — the right answer for a BOOT
// deciding whether to roll back, and the wrong one for a still-running
// process whose binary really is the target and whose .bak really is the
// rollback copy. Without the in-process term, the admin console could
// install the same target again and eat .bak, which is the defect this
// PR exists to fix, reached by waiting.
func TestTheGuardSurvivesAnExpiredRecencyWindow(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	livePath, upd, err := fix.install(t, "0.1.0")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	dir := filepath.Dir(livePath)

	// Age the marker past the window, exactly as a long-deferred manual
	// restart would.
	st, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.AttemptedAt = time.Now().Add(-RecencyWindow() - time.Hour)
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}

	if !upd.swapAwaitingRestart(dir, "0.2.0") {
		t.Fatal("the guard lapsed with the recency window while this process still holds the swap")
	}
	if _, err := upd.Install(context.Background(), retryOptsFor(livePath)); !errors.Is(err, ErrInstallPendingRestart) {
		t.Errorf("Install after the window lapsed: err = %v, want ErrInstallPendingRestart", err)
	}
	if got, want := bakBody(t, livePath), "bridge-binary-0.1.0"; got != want {
		t.Errorf("the rollback target was destroyed once the window lapsed: .bak = %q, want %q", got, want)
	}

	// A FRESH process past the window correctly reads the marker as
	// abandoned — that is the boot path's job, and the wedge test pins
	// the other half.
	fresh := New(Options{})
	if fresh.swapAwaitingRestart(dir, "0.2.0") {
		t.Error("a new process still refuses on an abandoned marker; the guard is a wedge")
	}
}
