package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B613's round 3: the marker's barrier asked again where a start
// short-cuts on what a failed barrier left live, one lock for the early
// rollback and every store open that migrates, and an interrupted rollback
// reconciled by the boot that finds it.

// boot runs the real serve on the install's settings and binary until it
// listens, for settings that load: the boot's housekeeping
// (maybeRollbackOnBoot) has run by the time it prints its banner.
func (r *refusedInstall) boot(t *testing.T) servedBridge {
	t.Helper()
	return launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin}, stdout, stderr)
	})
}

// bootAndStop boots serve as boot does, stops it, and returns what it said.
func (r *refusedInstall) bootAndStop(t *testing.T) string {
	t.Helper()
	sb := r.boot(t)
	sb.stop()
	if code := <-sb.done; code != 0 {
		t.Fatalf("serve = %d after its stop, want 0; stderr=%s", code, sb.stderr.String())
	}
	return sb.stderr.String()
}

// storeVersion is the migration step the store in dataDir is at.
func storeVersion(t *testing.T, dataDir string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", dsn.File(manifest.DefaultDBPath(dataDir), "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var v int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// latestStoreVersion is the step this build's ladder ends at.
func latestStoreVersion(t *testing.T) int {
	t.Helper()
	dir := t.TempDir()
	s, err := manifest.OpenStore(manifest.DefaultDBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return storeVersion(t, dir)
}

// withAnOlderStore puts the store in r's data dir one migration behind, made
// with no install armed, and arms r's marker again.
func (r *refusedInstall) withAnOlderStore(t *testing.T) {
	t.Helper()
	if err := updater.ClearState(r.dataDir); err != nil {
		t.Fatal(err)
	}
	storeOneMigrationBehind(t, r.dataDir)
	r.saveMarker(t, r.dataDir)
}

// TestServeRefusesUntilTheMarkersBarrierAnswers boots serve over a marker a
// barrier left live and not durable, a record (shape a) or a removal (shape
// b), with the barrier failing at each of its steps after the rename or the
// unlink: every boot refuses before its first write while the barrier
// fails, and the one after it answers goes on.
func TestServeRefusesUntilTheMarkersBarrierAnswers(t *testing.T) {
	for _, shape := range []struct {
		name string
		lay  func(t *testing.T, r *refusedInstall)
	}{
		{"a record", func(*testing.T, *refusedInstall) {}},
		{"a removal", func(t *testing.T, r *refusedInstall) {
			if err := updater.ClearState(r.dataDir); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(updater.StatePath(r.dataDir), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, step := range []string{"open", "sync", "close"} {
			t.Run(shape.name+"/"+step, func(t *testing.T) {
				r := newRefusedInstall(t, "")
				// Past the disarm, the boot ends at TLS: a certificate path
				// that is a directory.
				r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "tlsCertPath: "+r.dataDir+"\ntlsKeyPath: "+r.dataDir+"\n")
				shape.lay(t, r)
				failing := true
				t.Cleanup(updater.SetMarkerBarrierForTest(func(s string) error {
					if failing && s == step {
						return syscall.EIO
					}
					return nil
				}))
				for boot := 1; boot <= 3; boot++ {
					code, stderr := r.serve(t, serveOpts{})
					if code != 1 || strings.Contains(stderr, "TLS material") || !strings.Contains(updaterLines(stderr), "not starting") {
						t.Fatalf("boot %d while the barrier fails = %d, want 1 before its first write; stderr=%s", boot, code, stderr)
					}
				}
				requireNoStoreOpened(t, r.dataDir)
				failing = false
				if code, stderr := r.serve(t, serveOpts{}); code != 1 || !strings.Contains(stderr, "TLS material") {
					t.Fatalf("the boot once the barrier answers = %d, want it past the disarm, to TLS; stderr=%s", code, stderr)
				}
			})
		}
	}
}

// onceCloser closes its channel the first time it is asked.
type onceCloser struct {
	once sync.Once
	ch   chan struct{}
}

func newOnceCloser() *onceCloser { return &onceCloser{ch: make(chan struct{})} }

func (c *onceCloser) close() { c.once.Do(func() { close(c.ch) }) }

// signalOnce signals ch without blocking: a hook's first signal is all a
// test waits for.
func signalOnce(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// withinTheTest is how long these tests wait for a goroutine of their own to
// reach where it parks.
const withinTheTest = 20 * time.Second

// TestAMigrationThatHoldsTheMarkersLockFirstStopsTheRollback: a command of
// the new version (a `bridge doctor`, a scan) opens the store and migrates
// it, holding the install marker's lock while it records that it did, and
// the early rollback of a serve whose settings it refuses waits for that
// lock, reads the record, and leaves the install as it is.
func TestAMigrationThatHoldsTheMarkersLockFirstStopsTheRollback(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	r.withAnOlderStore(t)
	files := watch(t, r.bin, r.bak)
	latest := latestStoreVersion(t)

	held, release := newOnceCloser(), newOnceCloser()
	t.Cleanup(updater.SetBeforeDisarmRecordForTest(func(string) {
		held.close()
		<-release.ch
	}))
	waits := make(chan struct{}, 1)
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) { signalOnce(waits) }))

	ctx, cancel := context.WithCancel(context.Background())
	migrated := make(chan error, 1)
	migrationDone := make(chan struct{})
	go func() {
		defer close(migrationDone)
		s, err := manifest.OpenStore(manifest.DefaultDBPath(r.dataDir))
		if err == nil {
			err = s.Close()
		}
		migrated <- err
	}()
	drainLoopOnCleanup(t, cancel, migrationDone, "the migrating store open")
	select {
	case <-held.ch:
	case <-time.After(withinTheTest):
		t.Fatal("the migration never reached its record")
	}

	var stdout, stderr safeBuffer
	codes := make(chan int, 1)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		codes <- runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin}, &stdout, &stderr)
	}()
	drainLoopOnCleanup(t, cancel, serveDone, "the early rollback's serve")
	// Released after the drains run, so a failure leaves nothing parked.
	t.Cleanup(release.close)
	select {
	case <-waits:
	case <-serveDone:
		t.Fatalf("the rollback did not wait for the migration that held the marker's lock: serve = %d; stderr=%s", <-codes, stderr.String())
	case <-time.After(withinTheTest):
		t.Fatalf("the rollback never waited for the marker's lock; stderr=%s", stderr.String())
	}
	release.close()
	if err := <-migrated; err != nil {
		t.Fatalf("the migration = %v", err)
	}
	if code := <-codes; code != 2 {
		t.Fatalf("serve = %d, want 2: the store was migrated under the marker's lock first; stderr=%s", code, stderr.String())
	}
	files.requireUnchanged(t)
	if !strings.Contains(updaterLines(stderr.String()), "opened the data dir") {
		t.Errorf("serve did not say why it left the install; stderr=%s", stderr.String())
	}
	if v := storeVersion(t, r.dataDir); v != latest {
		t.Errorf("the store is at %d, want %d", v, latest)
	}
}

// TestARollbackThatHoldsTheMarkersLockFirstStopsTheMigration: the early
// rollback holds the install marker's lock from its read through the
// backup's rename, and a command of the version it rolls back, already
// running from its own image, waits for that lock and then refuses to
// migrate the store under the binary the rollback put back.
func TestARollbackThatHoldsTheMarkersLockFirstStopsTheMigration(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	r.withAnOlderStore(t)
	before := storeVersion(t, r.dataDir)

	atRestore, release := newOnceCloser(), newOnceCloser()
	restore := func(binaryPath, suffix string) error {
		atRestore.close()
		<-release.ch
		return updater.RollbackBinary(binaryPath, suffix)
	}
	waits := make(chan struct{}, 1)
	t.Cleanup(updater.SetStateLockWaitingForTest(func(string) { signalOnce(waits) }))

	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr safeBuffer
	codes := make(chan int, 1)
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		codes <- runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin, restoreBackup: restore,
			backupProbeTimeout: testBackupProbeTimeout}, &stdout, &stderr)
	}()
	drainLoopOnCleanup(t, cancel, serveDone, "the early rollback's serve")
	select {
	case <-atRestore.ch:
	case <-serveDone:
		t.Fatalf("serve = %d before its rename; stderr=%s", <-codes, stderr.String())
	case <-time.After(withinTheTest):
		t.Fatal("the rollback never reached its rename")
	}

	migrated := make(chan error, 1)
	migrationDone := make(chan struct{})
	go func() {
		defer close(migrationDone)
		s, err := manifest.OpenStore(manifest.DefaultDBPath(r.dataDir))
		if err == nil {
			err = s.Close()
		}
		migrated <- err
	}()
	drainLoopOnCleanup(t, cancel, migrationDone, "the migrating store open")
	t.Cleanup(release.close)
	select {
	case <-waits:
	case <-migrationDone:
		t.Fatalf("the migration did not wait for the rollback that held the marker's lock: %v", <-migrated)
	case <-time.After(withinTheTest):
		t.Fatal("the migration never waited for the marker's lock")
	}
	release.close()
	if code := <-codes; code != supervision.RestartExitCode {
		t.Fatalf("serve = %d, want %d; stderr=%s", code, supervision.RestartExitCode, stderr.String())
	}
	if err := <-migrated; !errors.Is(err, updater.ErrMigrationVetoed) {
		t.Fatalf("the migration after the rollback = %v, want it refused", err)
	}
	if v := storeVersion(t, r.dataDir); v != before {
		t.Errorf("the store is at %d, want %d: the migration ran under the version put back", v, before)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q, want %s", got, previousVersion)
	}
	if st, err := updater.LoadState(r.dataDir); err != nil || !st.RollbackStarted || st.DataDirOpenedBy != "" {
		t.Errorf("the marker = %+v (%v), want the rollback's record with no version recorded over it", st, err)
	}
}

// TestACommandOfTheVersionRolledBackRefusesToMigrateItsStore: a command of
// the version an early rollback undid (here `bridge scan`), over the
// rollback's record, refuses its store's migration and says why, whether or
// not the backup is back yet.
func TestACommandOfTheVersionRolledBackRefusesToMigrateItsStore(t *testing.T) {
	r := newRefusedInstall(t, "")
	if err := updater.ClearState(r.dataDir); err != nil {
		t.Fatal(err)
	}
	storeOneMigrationBehind(t, r.dataDir)
	r.marker = rollbackStoppedBeforeItsRename(r.marker)
	r.saveMarker(t, r.dataDir)
	before := storeVersion(t, r.dataDir)
	marker := watch(t, updater.StatePath(r.dataDir))
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"scan", "--config", r.cfg}, &stdout, &stderr); code == 0 {
		t.Fatalf("scan = 0 over a rollback of this version under way; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if v := storeVersion(t, r.dataDir); v != before {
		t.Errorf("the store is at %d, want %d", v, before)
	}
	marker.requireUnchanged(t)
	if !strings.Contains(stderr.String(), "bridge serve") {
		t.Errorf("scan did not say what finishes the rollback; stderr=%s", stderr.String())
	}
}

// TestATargetWhoseSettingsAreFixedConfirmsAnInterruptedRollback: a start of
// the version an early rollback was undoing, stopped before the backup was
// back, then started again on settings fixed since: its admission records
// that it opened the data dir before the store's migrations (pending here,
// the store a step behind), and its housekeeping confirms the install,
// however old the record, keeping the backup until the boot after it.
func TestATargetWhoseSettingsAreFixedConfirmsAnInterruptedRollback(t *testing.T) {
	latest := latestStoreVersion(t)
	for _, tc := range []struct {
		name string
		age  time.Duration
	}{
		{"recent", time.Minute},
		{"past the boot window", updater.RecencyWindow() + time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			if err := updater.ClearState(r.dataDir); err != nil {
				t.Fatal(err)
			}
			storeOneMigrationBehind(t, r.dataDir)
			r.marker.AttemptedAt = time.Now().UTC().Add(-tc.age)
			r.marker = rollbackStoppedBeforeItsRename(r.marker)
			r.saveMarker(t, r.dataDir)
			files := watch(t, r.bin, r.bak)
			// The store as the version before this one left it, genuinely:
			// the last migration's columns and index are not there yet (round
			// 4), so the restart runs that migration's own steps.
			requireTheLatestSchema(t, r.dataDir, false)

			r.bootAndStop(t)
			files.requireUnchanged(t)
			if v := storeVersion(t, r.dataDir); v != latest {
				t.Errorf("the store is at %d, want %d", v, latest)
			}
			requireTheLatestSchema(t, r.dataDir, true)
			st, err := updater.LoadState(r.dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if st.Status != "installed" || st.InstalledAt.IsZero() || st.RollbackStarted || st.DataDirOpenedBy != version.ServerVersion ||
				st.RejectedVersion != "" || st.RejectedReason != "" || st.RejectedConfigDigest != "" || st.MigrationVeto != "" {
				t.Fatalf("the marker after the boot = %+v, want the install confirmed, the rollback's mark, its hold of this version and its veto gone", st)
			}

			r.bootAndStop(t)
			if _, err := os.Lstat(r.bak); !os.IsNotExist(err) {
				t.Errorf("the backup after the boot that follows: lstat %v, want it cleaned up", err)
			}
			if st, err := updater.LoadState(r.dataDir); err != nil || st != (updater.State{}) {
				t.Errorf("the marker after the boot that follows = %+v (%v), want none", st, err)
			}
		})
	}
}

// TestARefusedStartFinishesAnInterruptedRollbackOnlyOnItsProof: a start of
// that version whose settings are still refused finishes the rollback only
// as the first start would have: recent, swapped, its backup the version the
// install replaced, and no version recorded over it; anything else leaves
// the install as it is.
func TestARefusedStartFinishesAnInterruptedRollbackOnlyOnItsProof(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lay      func(t *testing.T, r *refusedInstall)
		restores bool
		says     string
	}{
		{name: "every check holds", lay: func(*testing.T, *refusedInstall) {}, restores: true},
		{name: "past the boot window", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.AttemptedAt = time.Now().UTC().Add(-updater.RecencyWindow() - time.Hour)
		}, says: "older"},
		{name: "dated in the future", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.AttemptedAt = time.Now().UTC().Add(time.Hour)
		}, says: "future"},
		{name: "never swapped", lay: func(t *testing.T, r *refusedInstall) { r.marker.SwapStarted = false }, says: "swap"},
		{name: "a version recorded over it", lay: func(t *testing.T, r *refusedInstall) {
			r.marker.DataDirOpenedBy = version.ServerVersion
		}, says: "opened the data dir"},
		{name: "a backup of another version", lay: func(t *testing.T, r *refusedInstall) {
			t.Setenv(runAsBridgeVersionEnv, "0.0.0-another")
		}, says: "0.0.0-another"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "scanIntervalSec: 010\n")
			tc.lay(t, r)
			r.marker = rollbackStoppedBeforeItsRename(r.marker)
			r.saveMarker(t, r.dataDir)
			files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
			code, stderr := r.serve(t, serveOpts{})
			if tc.restores {
				if code != supervision.RestartExitCode || !strings.Contains(reportedVersion(t, r.bin), previousVersion) {
					t.Fatalf("serve = %d, want the rollback finished (%d); stderr=%s", code, supervision.RestartExitCode, stderr)
				}
				return
			}
			if code != 2 {
				t.Fatalf("serve = %d, want 2, the install left as it is; stderr=%s", code, stderr)
			}
			files.requireUnchanged(t)
			if !strings.Contains(updaterLines(stderr), tc.says) {
				t.Errorf("serve did not say why (%q); stderr=%s", tc.says, stderr)
			}
		})
	}
}

// TestAPatchedPreviousVersionKeepsTheHoldOfARollbackThatFinished: the
// rollback's record, and the version the backup held runs, a release with
// this round: the rollback was finished. Its boot clears the install the
// record describes and keeps the hold and the veto of the rolled-back
// version's store migrations (round 4), and never tries a restore.
func TestAPatchedPreviousVersionKeepsTheHoldOfARollbackThatFinished(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker = updater.State{Status: "installing", TargetVersion: "9.9.9", PreviousVersion: version.ServerVersion,
		AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true}
	r.marker = rollbackStoppedBeforeItsRename(r.marker)
	r.saveMarker(t, r.dataDir)
	files := watch(t, r.bin, r.bak)
	stderr := r.bootAndStop(t)
	files.requireUnchanged(t)
	st, err := updater.LoadState(r.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	want := updater.State{RejectedVersion: r.marker.RejectedVersion, RejectedReason: r.marker.RejectedReason,
		RejectedConfigDigest: r.marker.RejectedConfigDigest, MigrationVeto: "9.9.9"}
	if st != want {
		t.Errorf("the marker after the boot = %+v, want the hold and the veto alone, %+v", st, want)
	}
	if said := updaterLines(stderr); strings.Contains(said, "rolling back") || strings.Contains(said, "rollback failed") {
		t.Errorf("the boot tried a restore: %s", said)
	}
}
