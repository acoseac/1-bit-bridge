package main

import (
	"errors"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B872's Windows reds: on windows-latest the rename of the backup
// over the binary met a sharing violation, another process holding open the
// backup the start had just run (its version, and the crash rollback's
// settings check), and both of a start's rollbacks gave up at that first
// refusal: the crash rollback said "(restored); this start goes on" and
// served the release that crashed, and the early rollback refused its
// settings, exit 2, its data dir named relative to them. These tests run
// both through Windows' put-back of the backup
// (updater.ReplaceRunningBinaryForTest) on every host, its renames this
// file's: the backup in use for a moment, and in use for good.

// errBackupInUse stands in for the sharing violation Windows answers a
// rename of a file another process holds open.
var errBackupInUse = errors.New("The process cannot access the file because it is being used by another process.")

// backupInUse is a restore for serveOpts.restoreBackup: Windows' put-back of
// the backup over r's binary, whose rename of the backup finds it in use the
// first refused times (every time where refused is negative), and whose
// rename onto a binary still there is refused, as Windows refuses a rename
// onto a running image. Its waits are recorded, never slept; serve's
// goroutine writes them, and the test reads them once serve has returned.
type backupInUse struct {
	refused  int
	attempts atomic.Int32
	waits    []time.Duration
}

func (b *backupInUse) restore(r *refusedInstall) func(dst, ext string) error {
	return updater.ReplaceRunningBinaryForTest(func(oldpath, newpath string) error {
		if oldpath == r.bak && newpath == r.bin {
			if n := b.attempts.Add(1); b.refused < 0 || int(n) <= b.refused {
				return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errBackupInUse}
			}
		}
		if _, err := os.Lstat(newpath); err == nil {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EACCES}
		}
		return os.Rename(oldpath, newpath)
	}, func(err error) bool { return errors.Is(err, errBackupInUse) }, func(d time.Duration) { b.waits = append(b.waits, d) })
}

// requireThePutBack requires r's backup put back over its binary after its
// two refusals, waiting the schedule's first two waits: at its third rename
// of it, or on Windows at a later one where a scanner of the host's holds
// the backup the start has just run a moment more, which the rollback waits
// out too.
func (b *backupInUse) requireThePutBack(t *testing.T, r *refusedInstall) {
	t.Helper()
	n := int(b.attempts.Load())
	if n < 3 || (runtime.GOOS != "windows" && n != 3) {
		t.Errorf("the backup's renames = %d, want 3: refused twice as in use, then put back", n)
	}
	if want := []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}; len(b.waits) != n-1 || !slices.Equal(b.waits[:min(2, len(b.waits))], want) {
		t.Errorf("the rollback waited %v over %d renames, want %v first, one wait before each rename after the first", b.waits, n, want)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q after the rollback, want %s", got, previousVersion)
	}
	if _, err := os.Lstat(r.bak); !os.IsNotExist(err) {
		t.Errorf("the backup is still there (lstat %v): it was not put back", err)
	}
}

// requireInUseThroughout requires the backup's rename refused at each of the
// rollback's seven attempts, the schedule's every wait between them.
func (b *backupInUse) requireInUseThroughout(t *testing.T) {
	t.Helper()
	if n := b.attempts.Load(); n != 7 {
		t.Errorf("the backup's renames = %d, want 7, every attempt of the schedule", n)
	}
	if len(b.waits) != 6 {
		t.Errorf("the rollback waited %v, want the schedule's six waits", b.waits)
	}
}

// TestACrashRollbackPutsBackABackupInUseForAMoment: three starts of the
// install's version crashed, and at the next start's crash rollback another
// process holds the backup for a moment, refusing its first two renames: the
// rollback makes the rename again, puts the backup back and exits 75 before
// anything serves. On windows-latest the start served the release that
// crashed, "(restored); this start goes on" (TestACrashRollbackToAReleaseBeforeTheSwitchNeedsItOn).
func TestACrashRollbackPutsBackABackupInUseForAMoment(t *testing.T) {
	r := newRefusedInstall(t, "")
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	held := &backupInUse{refused: 2}
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin, restoreBackup: held.restore(r)})
	if served || code != supervision.RestartExitCode {
		t.Fatalf("the start after the crashes, its backup in use for a moment = %d (served %v), want %d before it served; stderr=%s",
			code, served, supervision.RestartExitCode, stderr)
	}
	held.requireThePutBack(t, r)
	if said := updaterLines(stderr); !strings.Contains(said, "did not end cleanly, so the install is rolled back") {
		t.Errorf("the start did not say it rolled the install back; its updater lines: %q", said)
	}
}

// TestACrashRollbackLeavesABackupInUseForGood: where the backup stays in use
// through every attempt, the crash rollback puts the running binary back
// from the aside and says so, "(restored)", records why it left the install,
// and the start goes on and serves, as it did before the retries; the
// binary is this version's still and the backup stays beside it.
func TestACrashRollbackLeavesABackupInUseForGood(t *testing.T) {
	r := newRefusedInstall(t, "")
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	binaries := watch(t, r.bin, r.bak)
	held := &backupInUse{refused: -1}
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams, restoreBackup: held.restore(r)})
	said := updaterLines(sb.stderr.String())
	for _, s := range []string{"could not put the backup back", "still in use after 7 attempts over 3.15s", "(restored)", crashRollbackGoesOn} {
		if !strings.Contains(said, s) {
			t.Errorf("the start did not say %q; its updater lines: %q", s, said)
		}
	}
	if st := markerOf(t, r.dataDir); st.RollbackRefused != "the backup could not be put back over the binary" || st.RollbackStarted {
		t.Errorf("the marker as the start serves = %+v, want why the backup was not put back, and no rollback under way", st)
	}
	stopServed(t, sb)
	held.requireInUseThroughout(t)
	binaries.requireUnchanged(t)
}

// TestTheEarlyRollbackPutsBackABackupInUseForAMoment: this version refuses
// its settings at its first start, and another process holds the backup for
// a moment as the early rollback puts it back: the rollback makes the
// rename again and exits 75, the version held back for these settings,
// whether the settings name the data dir relative to themselves (the case
// windows-latest failed, serve = 2) or not.
func TestTheEarlyRollbackPutsBackABackupInUseForAMoment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dataDir func(r *refusedInstall) string
	}{
		{"the data dir named relative to the settings", func(*refusedInstall) string { return "dataDir: data\n" }},
		{"the data dir named whole", func(r *refusedInstall) string { return "dataDir: " + r.dataDir + "\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.writeSettings(t, tc.dataDir(r), "scanIntervalSec: 010\n")
			held := &backupInUse{refused: 2}
			code, stderr := r.serve(t, serveOpts{restoreBackup: held.restore(r)})
			if code != supervision.RestartExitCode {
				t.Fatalf("serve = %d, want %d, its backup in use for a moment; updater lines: %q; stderr=%s",
					code, supervision.RestartExitCode, updaterLines(stderr), stderr)
			}
			held.requireThePutBack(t, r)
			st := markerOf(t, r.dataDir)
			if st.Status != "" || st.RejectedVersion != version.ServerVersion || st.RejectedConfigDigest == "" ||
				!strings.Contains(st.RejectedReason, "leading zero") {
				t.Errorf("the marker after the rollback = %+v, want this version held back for these settings", st)
			}
		})
	}
}

// TestTheEarlyRollbackLeavesABackupInUseForGood: where the backup stays in
// use through every attempt, the early rollback puts the running binary back
// from the aside, says so, "(restored)", and the start refuses its settings
// as before, exit 2, the binary, the backup and the install marker as they
// were, for the next start to ask again.
func TestTheEarlyRollbackLeavesABackupInUseForGood(t *testing.T) {
	r := newRefusedInstall(t, "scanIntervalSec: 010\n")
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	held := &backupInUse{refused: -1}
	code, stderr := r.serve(t, serveOpts{restoreBackup: held.restore(r)})
	if code != 2 {
		t.Fatalf("serve = %d, want 2, the settings refused as before; stderr=%s", code, stderr)
	}
	said := updaterLines(stderr)
	for _, s := range []string{"not rolling back the install", "could not put the backup back", "still in use after 7 attempts over 3.15s", "(restored)"} {
		if !strings.Contains(said, s) {
			t.Errorf("serve did not say %q; its updater lines: %q", s, said)
		}
	}
	held.requireInUseThroughout(t)
	files.requireUnchanged(t)
}
