package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B705: the installed version's first start that comes more than the
// boot window after its install completes the install, and no step of the
// boot's housekeeping drops the record that a version other than the
// backup's opened the data dir while the backup stays beside the binary. An
// operator's rollback refuses on that record (updater.RollbackRefusal); with
// it gone, the console's Roll back put the backup back over a store the new
// version had migrated.

// rollBackNow runs the operator's rollback of r's install, as the console's
// does, to its answer.
func rollBackNow(t *testing.T, r *refusedInstall) error {
	t.Helper()
	return updater.New(updater.Options{}).Rollback(t.Context(),
		updater.InstallOptions{DataDir: r.dataDir, BinaryPath: r.bin, Force: true})
}

// watchInstall watches r's binary, its backup and, where there is one, its
// install marker.
func watchInstall(t *testing.T, r *refusedInstall) watchedFiles {
	t.Helper()
	paths := []string{r.bin, r.bak}
	if _, err := os.Lstat(updater.StatePath(r.dataDir)); err == nil {
		paths = append(paths, updater.StatePath(r.dataDir))
	}
	return watch(t, paths...)
}

// TestALateFirstStartOfTheInstalledVersionCompletesItsInstall: `bridge update`
// with the service stopped and a start the next day. This build's real serve
// starts over an install of it armed 30 hours ago and a store one migration
// behind: its admission records that it opened the data dir, its store's open
// migrates, its housekeeping puts the install on probation whatever its age,
// and the start confirms it once it has answered and stayed up (backlog B660:
// until B660 the housekeeping confirmed it), keeping the record and the
// backup. The console's Roll back then refuses, both binaries as they are,
// since the backup's version does not read the store as it is now; and the
// start after it removes the backup, as after any confirmed install.
func TestALateFirstStartOfTheInstalledVersionCompletesItsInstall(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.AttemptedAt = time.Now().UTC().Add(-30 * time.Hour)
	r.withAnOlderStore(t)
	latest := latestStoreVersion(t)
	requireTheLatestSchema(t, r.dataDir, false)
	binaries := watch(t, r.bin, r.bak)

	events, seams := newProbationWatch(shortProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	if st := markerOf(t, r.dataDir); st.Status != "installing" || st.DataDirOpenedBy != version.ServerVersion {
		t.Errorf("the marker once the late start listens = %+v, want the install on probation, whatever its age", st)
	}
	events.waitFor(t, probationConfirmed)
	stopServed(t, sb)
	stderr := sb.stderr.String()
	binaries.requireUnchanged(t)
	if v := storeVersion(t, r.dataDir); v != latest {
		t.Errorf("the store after the late start is at %d, want %d", v, latest)
	}
	requireTheLatestSchema(t, r.dataDir, true)
	st, err := updater.LoadState(r.dataDir)
	if err != nil || st.Status != "installed" || st.InstalledAt.IsZero() || st.TargetVersion != version.ServerVersion ||
		st.PreviousVersion != previousVersion || st.DataDirOpenedBy != version.ServerVersion {
		t.Errorf("the marker after the late start = %+v (%v), want the install confirmed with the record that %s opened the data dir; "+
			"stderr=%s", st, err, version.ServerVersion, updaterLines(stderr))
	}

	files := watchInstall(t, r)
	if err := rollBackNow(t, r); !errors.Is(err, updater.ErrRollbackRefused) {
		t.Fatalf("the console's rollback after the late start = %v, want it refused: %s migrated the store, and the backup is %s",
			err, version.ServerVersion, previousVersion)
	}
	files.requireUnchanged(t)

	stderr = r.bootAndStop(t)
	if _, err := os.Lstat(r.bak); !os.IsNotExist(err) {
		t.Errorf("the backup after the start that follows: lstat %v, want it removed; stderr=%s", err, updaterLines(stderr))
	}
	if st, err := updater.LoadState(r.dataDir); err != nil || st != (updater.State{}) {
		t.Errorf("the marker after the start that follows = %+v (%v), want none", st, err)
	}
	if got, err := os.ReadFile(r.bin); err != nil || string(got) != "the new bridge binary" {
		t.Errorf("the binary after the start that follows = %q (%v), want the install's", got, err)
	}
}

// TestAFailedRemovalOfTheBackupLeavesTheInstallForTheNextStart: the start
// after a confirmed install removes the backup, and a removal that fails (on
// Windows one can) leaves the install marker as it is, with its record that
// the version installed opened the data dir: the console's Roll back
// meanwhile refuses on it, and the next start removes the backup and clears
// the marker.
func TestAFailedRemovalOfTheBackupLeavesTheInstallForTheNextStart(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.marker.Status, r.marker.InstalledAt = "installed", time.Now().UTC().Add(-time.Minute)
	r.marker.DataDirOpenedBy = version.ServerVersion
	r.saveMarker(t, r.dataDir)
	files := watchInstall(t, r)

	var stderr bytes.Buffer
	refused := errors.New("the file is being used by another process")
	if err := bootHousekeepingWith(t.Context(), &stderr, r.dataDir, r.bin, version.ServerVersion, nil,
		func(string, string) error { return refused }); err != nil {
		t.Fatalf("the housekeeping = %v; stderr=%s", err, stderr.String())
	}
	if st, err := updater.LoadState(r.dataDir); err != nil || st.Status != "installed" || st.DataDirOpenedBy != version.ServerVersion {
		t.Errorf("the marker after a removal of the backup that failed = %+v (%v), want it as it was; stderr=%s", st, err, stderr.String())
	}
	if !strings.Contains(updaterLines(stderr.String()), refused.Error()) {
		t.Errorf("the housekeeping did not say the removal failed; stderr=%s", stderr.String())
	}
	if err := rollBackNow(t, r); !errors.Is(err, updater.ErrRollbackRefused) {
		t.Fatalf("the console's rollback after a removal that failed = %v, want it refused: %s opened the data dir", err, version.ServerVersion)
	}
	files.requireUnchanged(t)

	stderr.Reset()
	if err := bootHousekeeping(t.Context(), &stderr, r.dataDir, r.bin, version.ServerVersion, nil); err != nil {
		t.Fatalf("the next start's housekeeping = %v; stderr=%s", err, stderr.String())
	}
	if _, err := os.Lstat(r.bak); !os.IsNotExist(err) {
		t.Errorf("the backup after the next start: lstat %v, want it removed; stderr=%s", err, stderr.String())
	}
	if st, err := updater.LoadState(r.dataDir); err != nil || st != (updater.State{}) {
		t.Errorf("the marker after the next start = %+v (%v), want none", st, err)
	}
}

// TestAStartOfAnotherVersionKeepsTheRecordOfALateStartThatDied is Argon's
// late start that dies before its housekeeping: the version installed starts,
// its admission records that it opened the data dir, its store migrates, and
// it ends before the housekeeping; the operator puts the previous binary back
// by hand, keeping the backup, and starts it. That start's housekeeping reads
// an install that did not come up as its version, inside the boot window or
// past it, and leaves the backup where it is, as before; it keeps the record,
// so the console's Roll back refuses afterwards.
func TestAStartOfAnotherVersionKeepsTheRecordOfALateStartThatDied(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
	}{
		{"inside the boot window", time.Minute},
		{"past the boot window", updater.RecencyWindow() + time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.marker.AttemptedAt = time.Now().UTC().Add(-tc.age)
			r.withAnOlderStore(t)
			res, err := updater.AdmitBoot(t.Context(), r.dataDir, version.ServerVersion, nil)
			if err != nil || !res.Recorded {
				t.Fatalf("the admission of the version installed = %+v, %v; want its record", res, err)
			}
			// Serve's own open, with its admission's permission (B660's
			// second round).
			s, err := manifest.OpenStoreAdmitted(manifest.DefaultDBPath(r.dataDir), res.Permit)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			requireTheLatestSchema(t, r.dataDir, true)
			if err := os.WriteFile(r.bin, []byte("the previous bridge binary, put back by hand"), 0o755); err != nil {
				t.Fatal(err)
			}

			var stderr bytes.Buffer
			if err := bootHousekeeping(t.Context(), &stderr, r.dataDir, r.bin, previousVersion, nil); err != nil {
				t.Fatalf("the previous version's housekeeping = %v; stderr=%s", err, stderr.String())
			}
			st, err := updater.LoadState(r.dataDir)
			if err != nil || updater.InstallArmed(st) || st.DataDirOpenedBy != version.ServerVersion || st.PreviousVersion != previousVersion {
				t.Errorf("the marker after the previous version's start = %+v (%v), want the install cleared and the record that %s "+
					"opened the data dir kept; stderr=%s", st, err, version.ServerVersion, stderr.String())
			}
			files := watchInstall(t, r)
			if err := rollBackNow(t, r); !errors.Is(err, updater.ErrRollbackRefused) {
				t.Fatalf("the console's rollback after the previous version's start = %v, want it refused: %s migrated the store",
					err, version.ServerVersion)
			}
			files.requireUnchanged(t)
		})
	}
}
