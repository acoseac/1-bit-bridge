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

// Backlog B692's install half: Install takes the install marker's lock only
// after the download, its verification and the settings check, decides again
// under it, and holds it through its marker writes and the swap. Not on
// Windows, as every install test here: the fixture's archive is a tar.gz
// holding `bridge`.

// installingUpdater is an Updater of version current that has polled fix's
// release, as a process about to install it.
func installingUpdater(t *testing.T, fix *installFixture, current string) *Updater {
	t.Helper()
	upd := New(Options{RepoOverride: "fake/repo", Client: NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL)})
	upd.mu.Lock()
	upd.status.CurrentVersion, upd.status.LatestVersion, upd.status.UpdateAvailable = current, fix.latestVersion, true
	upd.mu.Unlock()
	return upd
}

// installLayout is a data dir and a binary of version 0.2.1 beside it.
func installLayout(t *testing.T) (dataDir, live string) {
	t.Helper()
	dataDir = t.TempDir()
	live = filepath.Join(dataDir, "bridge")
	if err := os.WriteFile(live, []byte("bridge-binary-0.2.1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dataDir, live
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// installIn runs Install on a goroutine.
func installIn(t *testing.T, ctx context.Context, upd *Updater, opts InstallOptions, unhold func()) (<-chan error, <-chan struct{}) {
	t.Helper()
	answer, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		_, err := upd.Install(ctx, opts)
		answer <- err
	}()
	drainOnCleanup(t, unhold, done, "the install")
	return answer, done
}

// TestAnInstallThatHoldsTheMarkersLockFirstKeepsTheMigrationWaiting: an
// install holds the install marker's lock from its decision through its
// marker writes and its swap, so a store open that migrates, arriving after
// the install armed its marker and before its swap, waits for it and then
// records over the install's whole marker: armed, swapped, and the record of
// the data dir beside it. Without the lock the migration recorded over the
// marker before the swap, and the install's record of its swap, written from
// its own copy, took the migration's record away.
func TestAnInstallThatHoldsTheMarkersLockFirstKeepsTheMigrationWaiting(t *testing.T) {
	fix := newInstallFixture(t, "0.2.3")
	dir, live := installLayout(t)
	atSwap, release := newGate(), newGate()
	waits := signalWaits(t)
	installed, _ := installIn(t, context.Background(), installingUpdater(t, fix, "0.2.1"), InstallOptions{
		DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier,
		holdBeforeSwap: func() { atSwap.open(); <-release.ch },
	}, release.open)
	awaitGate(t, atSwap, "the install's swap")

	migrated, migrationDone := migrateIn(t, dir, "0.2.3", release.open)
	select {
	case <-waits:
	case <-migrationDone:
		t.Fatalf("the migration did not wait for the install that held the marker's lock: %+v", <-migrated)
	case <-time.After(withinTheTest):
		t.Fatal("the migration never waited for the marker's lock")
	}
	release.open()
	if err := <-installed; err != nil {
		t.Fatalf("Install = %v", err)
	}
	if m := <-migrated; m.err != nil || !m.res.Recorded {
		t.Fatalf("the migration after the install = %+v, want its record made over the install", m)
	}
	st, err := LoadState(dir)
	if err != nil || st.Status != "installing" || st.TargetVersion != "0.2.3" || !st.SwapStarted || st.DataDirOpenedBy != "0.2.3" {
		t.Errorf("the marker = %+v (%v), want the install armed and swapped with the migration's record beside it", st, err)
	}
}

// TestAMigrationThatHoldsTheMarkersLockFirstIsReadByTheInstallsDecision: two
// processes share no installInFlight. The console's install (B) gets past its
// first check of the marker; then `bridge update` (A) installs the same
// release, and a command of the new binary starts migrating the store and
// holds the install marker's lock at its record. B waits for that lock, and
// its decision, made again under it, reads A's install and the record beside
// it: the swap waits for a restart, so B swaps nothing and records nothing,
// the backup of the version A replaced and the migration's record intact.
func TestAMigrationThatHoldsTheMarkersLockFirstIsReadByTheInstallsDecision(t *testing.T) {
	fix := newInstallFixture(t, "0.2.4")
	dir, live := installLayout(t)
	inVerify, resume := newGate(), newGate()
	b, _ := installIn(t, context.Background(), installingUpdater(t, fix, "0.2.1"), InstallOptions{
		DataDir: dir, BinaryPath: live, Force: true,
		Verifier: func(context.Context, string) error { inVerify.open(); <-resume.ch; return nil },
	}, resume.open)
	awaitGate(t, inVerify, "the console's install's verification")

	if _, err := installingUpdater(t, fix, "0.2.1").Install(context.Background(),
		InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier}); err != nil {
		t.Fatalf("`bridge update`'s install = %v", err)
	}
	atRecord, recorded := newGate(), newGate()
	t.Cleanup(SetBeforeDisarmRecordForTest(func(string) { atRecord.open(); <-recorded.ch }))
	migrated, _ := migrateIn(t, dir, "0.2.4", recorded.open)
	awaitGate(t, atRecord, "the migration's record")

	waits := signalWaits(t)
	resume.open()
	select {
	case <-waits:
	case err := <-b:
		t.Fatalf("the console's install did not wait for the migration that held the marker's lock: %v", err)
	case <-time.After(withinTheTest):
		t.Fatal("the console's install never waited for the marker's lock")
	}
	recorded.open()
	if m := <-migrated; m.err != nil || !m.res.Recorded {
		t.Fatalf("the migration = %+v, want its record made", m)
	}
	if err := <-b; !errors.Is(err, ErrInstallPendingRestart) {
		t.Fatalf("the console's install after `bridge update`'s = %v, want it refused as waiting for a restart", err)
	}
	if got := readFile(t, live+".bak"); got != "bridge-binary-0.2.1" {
		t.Errorf("the backup is %q, want the version `bridge update` replaced", got)
	}
	if got := readFile(t, live); got != "bridge-binary-0.2.4" {
		t.Errorf("the binary is %q, want `bridge update`'s install", got)
	}
	if st, err := LoadState(dir); err != nil || st.TargetVersion != "0.2.4" || !st.SwapStarted || st.DataDirOpenedBy != "0.2.4" {
		t.Errorf("the marker = %+v (%v), want `bridge update`'s install with the migration's record", st, err)
	}
}

// TestASecondProcesssInstallDecidesAgainUnderTheMarkersLock: the second of
// two processes to reach the swap with the same release (serve's install and
// `bridge update` share no installInFlight) reads, under the install marker's
// lock, the first one's install waiting for its restart, and stops there:
// nothing swapped, so the backup still holds the version the first replaced.
func TestASecondProcesssInstallDecidesAgainUnderTheMarkersLock(t *testing.T) {
	fix := newInstallFixture(t, "0.2.4")
	dir, live := installLayout(t)
	inVerify, resume := newGate(), newGate()
	second, _ := installIn(t, context.Background(), installingUpdater(t, fix, "0.2.1"), InstallOptions{
		DataDir: dir, BinaryPath: live, Force: true,
		Verifier: func(context.Context, string) error { inVerify.open(); <-resume.ch; return nil },
	}, resume.open)
	awaitGate(t, inVerify, "the second install's verification")
	if _, err := installingUpdater(t, fix, "0.2.1").Install(context.Background(),
		InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier}); err != nil {
		t.Fatalf("the first install = %v", err)
	}
	before := readFile(t, StatePath(dir))
	resume.open()
	if err := <-second; !errors.Is(err, ErrInstallPendingRestart) {
		t.Fatalf("the second install = %v, want it refused as waiting for a restart", err)
	}
	if got := readFile(t, live+".bak"); got != "bridge-binary-0.2.1" {
		t.Errorf("the backup is %q after the second install, want the version the first replaced", got)
	}
	if after := readFile(t, StatePath(dir)); after != before {
		t.Errorf("the second install wrote the marker: %s", after)
	}
}

// TestAnAutomaticInstallDecidesAgainOnAHoldRecordedWhileItDownloaded: a hold
// of the candidate recorded by another process after the auto-installer's
// gate (`bridge update`'s refused check, against the same settings) is read
// by its decision under the install marker's lock, and nothing is swapped.
func TestAnAutomaticInstallDecidesAgainOnAHoldRecordedWhileItDownloaded(t *testing.T) {
	fix := newInstallFixture(t, "0.2.4")
	dir, live := installLayout(t)
	cfg := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfg, []byte("scanIntervalSec: 010\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := ConfigDigest(cfg)
	if err != nil {
		t.Fatal(err)
	}
	opts := InstallOptions{
		DataDir: dir, BinaryPath: live, Force: true, Automatic: true, ConfigPath: cfg,
		Verifier: func(context.Context, string) error {
			return recordRejection(dir, "0.2.4", "0.2.4 refuses this bridge's settings: line 1: x", digest)
		},
		ConfigCheck: func(context.Context, string, string) ConfigCheck { return ConfigCheck{Verdict: ConfigValid} },
	}
	_, err = installingUpdater(t, fix, "0.2.1").Install(context.Background(), opts)
	if _, held := RefusalReason(err); !held {
		t.Fatalf("the automatic install over a hold recorded meanwhile = %v, want the hold", err)
	}
	if got := readFile(t, live); got != "bridge-binary-0.2.1" {
		t.Errorf("the binary is %q, want it as it was", got)
	}
	if st, err := LoadState(dir); err != nil || st.Status != "" || st.RejectedVersion != "0.2.4" {
		t.Errorf("the marker = %+v (%v), want the hold alone", st, err)
	}
}

// TestAnInstallRefusedForTheMarkersLockSwapsNothing: an install that cannot
// hold the install marker's lock (another process past the wait, no lock to
// give, a failure, a filesystem that keeps none) refuses before its marker
// write and its swap, naming no path; one stopped while it waits, or stopped
// before an uncontended take, returns the stop; either way nothing is armed,
// swapped or left beside the binary.
func TestAnInstallRefusedForTheMarkersLockSwapsNothing(t *testing.T) {
	requireNothingInstalled := func(t *testing.T, dir, live string) {
		t.Helper()
		if got := readFile(t, live); got != "bridge-binary-0.2.1" {
			t.Errorf("the binary is %q, want it as it was", got)
		}
		if _, err := os.Lstat(live + ".bak"); !os.IsNotExist(err) {
			t.Errorf("a backup is beside the binary (%v): something was swapped", err)
		}
		if _, err := os.Lstat(StatePath(dir)); !os.IsNotExist(err) {
			t.Errorf("the marker was written (%v)", err)
		}
		entries, _ := os.ReadDir(filepath.Dir(live))
		for _, e := range entries {
			if n := e.Name(); n != "bridge" && n != stateLockFileName {
				t.Errorf("%s is left beside the binary", n)
			}
		}
	}
	for _, tc := range theMarkerLockRefusals {
		t.Run(tc.name, func(t *testing.T) {
			fix := newInstallFixture(t, "0.2.3")
			dir, live := installLayout(t)
			tc.lay(t, dir)
			_, err := installingUpdater(t, fix, "0.2.1").Install(context.Background(),
				InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier})
			requireAMarkerLockRefusal(t, err, dir)
			requireNothingInstalled(t, dir, live)
		})
	}
	t.Run("stopped while it waits", func(t *testing.T) {
		fix := newInstallFixture(t, "0.2.3")
		dir, live := installLayout(t)
		holdStateLock(t, dir, withinTheTest)
		waits := signalWaits(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		installed, _ := installIn(t, ctx, installingUpdater(t, fix, "0.2.1"),
			InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier}, cancel)
		select {
		case <-waits:
		case err := <-installed:
			t.Fatalf("the install did not wait for the marker's lock another process held: %v", err)
		case <-time.After(withinTheTest):
			t.Fatal("the install never waited for the marker's lock")
		}
		cancel()
		if err := <-installed; !errors.Is(err, context.Canceled) || errors.Is(err, ErrMarkerLockUnavailable) {
			t.Fatalf("an install stopped in its wait = %v, want the stop and no refusal of the lock's", err)
		}
		requireNothingInstalled(t, dir, live)
	})
	t.Run("stopped before an uncontended take", func(t *testing.T) {
		fix := newInstallFixture(t, "0.2.3")
		dir, live := installLayout(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The stop lands once the download is verified: the lock is free,
		// and taken whatever the context says.
		verifier := func(context.Context, string) error { cancel(); return nil }
		_, err := installingUpdater(t, fix, "0.2.1").Install(ctx,
			InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: verifier})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("an install stopped before its lock = %v, want the stop", err)
		}
		requireNothingInstalled(t, dir, live)
	})
}

// TestAnInstallWhoseSwapFailsPutsTheMarkerBackAsItReadIt: a swap that fails
// before it changes anything leaves the binaries as they were, so the install
// puts back the marker it read under the install marker's lock, a hold and a
// veto an earlier rollback recorded included (backlog B692), and none where
// there was none, as the swap's failure always left it.
func TestAnInstallWhoseSwapFailsPutsTheMarkerBackAsItReadIt(t *testing.T) {
	failTheSwap := func(t *testing.T) {
		origLink, origRename := linkFunc, renameFunc
		linkFunc = func(string, string) error { return errors.New("operation not supported") }
		renameFunc = func(oldname, newname string) error {
			if filepath.Ext(newname) == ".bak" {
				return errors.New("rename refused")
			}
			return os.Rename(oldname, newname)
		}
		t.Cleanup(func() { linkFunc, renameFunc = origLink, origRename })
	}
	for _, tc := range []struct {
		name  string
		prior *State
	}{
		{"over a rollback's hold and veto", &State{RejectedVersion: "0.2.0", MigrationVeto: "0.2.0"}},
		{"over no marker", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix := newInstallFixture(t, "0.2.3")
			dir, live := installLayout(t)
			if tc.prior != nil {
				if err := SaveState(dir, *tc.prior); err != nil {
					t.Fatal(err)
				}
			}
			before := readFile(t, StatePath(dir))
			failTheSwap(t)
			if _, err := installingUpdater(t, fix, "0.2.1").Install(context.Background(),
				InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier}); err == nil {
				t.Fatal("Install over a swap that fails = nil, want the swap's failure")
			}
			if after := readFile(t, StatePath(dir)); after != before {
				t.Errorf("the marker after the failed swap = %q, want it as the install read it, %q", after, before)
			}
			if got := readFile(t, live); got != "bridge-binary-0.2.1" {
				t.Errorf("the binary is %q, want it as it was", got)
			}
		})
	}
}

// TestTheHoldOfARefusedCandidateIsRecordedOnlyUnderTheMarkersLock: the
// settings check's refusal records the version held back by a read, a change
// and a write of the marker holding the install marker's lock; where it
// cannot hold it, the install is refused all the same and nothing is
// recorded, so the next check asks again.
func TestTheHoldOfARefusedCandidateIsRecordedOnlyUnderTheMarkersLock(t *testing.T) {
	refused := func(context.Context, string, string) ConfigCheck {
		return ConfigCheck{Verdict: ConfigRefused, Reasons: []string{"line 1: a whole number is written with a leading zero"}}
	}
	run := func(t *testing.T, lay func(t *testing.T, dir string)) (dir string, err error) {
		fix := newInstallFixture(t, "0.2.3")
		dir, live := installLayout(t)
		cfg := filepath.Join(dir, "bridge.yaml")
		if err := os.WriteFile(cfg, []byte("scanIntervalSec: 010\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(dir, State{MigrationVeto: "0.2.2"}); err != nil {
			t.Fatal(err)
		}
		lay(t, dir)
		_, err = installingUpdater(t, fix, "0.2.1").Install(context.Background(), InstallOptions{
			DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier, ConfigPath: cfg, ConfigCheck: refused,
		})
		return dir, err
	}
	for _, tc := range theMarkerLockRefusals {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := run(t, tc.lay)
			if !errors.Is(err, ErrConfigRefused) {
				t.Fatalf("Install = %v, want the settings' refusal", err)
			}
			if st, err := LoadState(dir); err != nil || st.RejectedVersion != "" || st.MigrationVeto != "0.2.2" {
				t.Errorf("the marker = %+v (%v), want nothing recorded without the lock", st, err)
			}
		})
	}
	t.Run("the lock free", func(t *testing.T) {
		dir, err := run(t, func(*testing.T, string) {})
		if !errors.Is(err, ErrConfigRefused) {
			t.Fatalf("Install = %v, want the settings' refusal", err)
		}
		if st, err := LoadState(dir); err != nil || st.RejectedVersion != "0.2.3" || st.MigrationVeto != "0.2.2" {
			t.Errorf("the marker = %+v (%v), want the hold recorded and the rest kept", st, err)
		}
	})
}
