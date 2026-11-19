package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B894 (B898's F1): the crash rollback asks its backup its version and
// its settings check before it takes the install marker's lock, and decides
// again under that lock, on the marker read again, the backup its runs asked
// and the settings its check answered for, so that no run of the backup holds
// the lock: a settings check that did not answer held it 32 s, past the 30 s
// every other taker waits, at every start of a crash loop. These tests call
// the rollback itself, its backup's runs stood in for (backupRuns), each
// asking whether the lock is held as it runs and changing what its case
// changes there.

// markersLockHeld reports whether the install marker's lock in dataDir is held
// as it asks: it tries the lock through an open of the lock file of its own,
// lets a take go at once, and closes the file either way. The lock is a kernel
// lock on an open file, which refuses a second open in this process as it
// refuses another process's (fsutil's TestTryLockRefusesASecondOpenOfTheFile,
// on every platform), so a crash rollback that holds it is found.
func markersLockHeld(t *testing.T, dataDir string) bool {
	t.Helper()
	lock := updater.StateLockPath(dataDir)
	f, err := fsutil.OpenLockFile(filepath.Dir(lock), filepath.Base(lock))
	if err != nil {
		t.Fatalf("open the install marker's lock file: %v", err)
	}
	defer f.Close()
	switch err := fsutil.TryLock(f); {
	case err == nil:
		if err := fsutil.Unlock(f); err != nil {
			t.Fatalf("let the install marker's lock go: %v", err)
		}
		return false
	case errors.Is(err, fsutil.ErrLocked):
		return true
	default:
		t.Fatalf("try the install marker's lock: %v", err)
		return false
	}
}

// backupBytes are the bytes of the backup crashInstallAtTheLimit lays out.
const backupBytes = "the previous bridge binary"

// crashInstallAtTheLimit is an install whose count asks for the crash
// rollback, its marker as the pin of the lock's wait lays it
// (TestEveryRollbackAtAStartFitsInsideTheMarkersLockWait):
// updater.ProbationFailedStarts starts of this version counted, each recorded
// as having opened the data dir. Its binary and its backup are small files of
// its own, the backup backupBytes, which its cases rewrite and replace, and
// nothing runs either, the backup's runs being stood in for: never
// newRefusedInstall's backup (installBridgeCopy), a hard link to the running
// test binary where the host makes one, and on Windows a copy of it, which a
// scanner may still hold when a case would replace it.
func crashInstallAtTheLimit(t *testing.T) *refusedInstall {
	t.Helper()
	dir := t.TempDir()
	r := &refusedInstall{dir: dir, cfg: filepath.Join(dir, "bridge.yaml"), dataDir: filepath.Join(dir, "data")}
	r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "")
	name := "bridge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	r.bin = filepath.Join(dir, "bin", name)
	r.bak = r.bin + ".bak"
	for _, d := range []string{r.dataDir, filepath.Dir(r.bin)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, b := range map[string]string{r.bin: "the new bridge binary", r.bak: backupBytes} {
		if err := os.WriteFile(path, []byte(b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.marker = updater.State{
		Status: "installing", TargetVersion: version.ServerVersion, PreviousVersion: previousVersion,
		AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true,
		FailedStarts: updater.ProbationFailedStarts, DataDirOpenedBy: version.ServerVersion,
	}
	r.saveMarker(t, r.dataDir)
	return r
}

// backupRuns stands in for the crash rollback's two runs of its backup, its
// `version` (serveOpts.probeVersion) and its settings check
// (serveOpts.checkConfig), on the goroutine the rollback runs on, the test's:
// each records whether it found the install marker's lock held, then does what
// version or check does and answers what it answers, or, where that is unset,
// answers as a backup of previousVersion that loads the settings. identity,
// where set, is the rollback's read of its backup's identity
// (serveOpts.backupIdentity).
type backupRuns struct {
	dataDir  string
	version  func(ctx context.Context) (string, error)
	check    func(ctx context.Context) updater.ConfigCheck
	identity func(path string) (os.FileInfo, error)
	asked    []string
}

// lockFound is how backupRuns records what a run found.
func lockFound(held bool) string {
	if held {
		return "with the install marker's lock held"
	}
	return "with the install marker's lock free"
}

// opts are serve's options for a crash rollback over r whose backup's runs
// are b's and whose restore is restore.
func (b *backupRuns) opts(t *testing.T, r *refusedInstall, restore func(dst, ext string) error) serveOpts {
	t.Helper()
	b.dataDir = r.dataDir
	return serveOpts{configPath: r.cfg, binaryPath: r.bin, restoreBackup: restore, backupIdentity: b.identity,
		probeVersion: func(ctx context.Context, _ string, _ time.Duration) (string, error) {
			b.asked = append(b.asked, "the backup's version "+lockFound(markersLockHeld(t, b.dataDir)))
			if b.version != nil {
				return b.version(ctx)
			}
			return previousVersion, nil
		},
		checkConfig: func(ctx context.Context, _, _ string) updater.ConfigCheck {
			b.asked = append(b.asked, "the backup's settings check "+lockFound(markersLockHeld(t, b.dataDir)))
			if b.check != nil {
				return b.check(ctx)
			}
			return updater.ConfigCheck{Verdict: updater.ConfigValid}
		}}
}

// crashRollback is what one crash rollback did: what it answered and said,
// and the install marker as it stood at each rename of its restore.
type crashRollback struct {
	code    int
	acted   bool
	stderr  string
	renames []updater.State
}

// crashRollBack runs the crash rollback over r on ctx, with b's runs and a
// restore that records the install marker at its rename and then puts the
// backup back as production does.
func crashRollBack(ctx context.Context, t *testing.T, r *refusedInstall, b *backupRuns) crashRollback {
	t.Helper()
	var got crashRollback
	restore := func(dst, ext string) error {
		got.renames = append(got.renames, markerOf(t, r.dataDir))
		return updater.RollbackBinary(dst, ext)
	}
	var stderr bytes.Buffer
	got.code, got.acted = recoverCrashedInstall(ctx, r.dataDir, r.cfg, b.opts(t, r, restore), crashRollbackGoesOn, &stderr)
	got.stderr = stderr.String()
	return got
}

// requirePutBack requires the crash rollback to have put r's backup back:
// exit 75, its record durable before its one rename, the backup's bytes at
// the binary's path and nothing left beside it.
func requirePutBack(t *testing.T, r *refusedInstall, got crashRollback) {
	t.Helper()
	if got.code != supervision.RestartExitCode || !got.acted {
		t.Fatalf("the crash rollback = %d (acted %v), want %d; stderr=%s", got.code, got.acted, supervision.RestartExitCode, got.stderr)
	}
	if len(got.renames) != 1 {
		t.Fatalf("the backup was put back by %d renames, want one", len(got.renames))
	}
	if at := got.renames[0]; at.Status != "" || !at.RollbackStarted || at.RejectedVersion != version.ServerVersion ||
		at.MigrationVeto != version.ServerVersion || at.FailedStarts != updater.ProbationFailedStarts {
		t.Errorf("the marker as the backup was put back = %+v, want the crash rollback's record", at)
	}
	if b, err := os.ReadFile(r.bin); err != nil || string(b) != backupBytes {
		t.Errorf("the binary after the rollback = %q (%v), want the backup's bytes", b, err)
	}
	if _, err := os.Lstat(r.bak); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the backup is still there (lstat %v): it was not put back", err)
	}
}

// requireLeft requires the crash rollback to have left r's install: no exit,
// no rename, the files binaries watches as they were, and no rollback under
// way in the marker.
func requireLeft(t *testing.T, r *refusedInstall, got crashRollback, binaries watchedFiles) {
	t.Helper()
	if got.code != 0 || got.acted || len(got.renames) != 0 {
		t.Errorf("the crash rollback = %d (acted %v) after %d renames, want it to leave the install; stderr=%s",
			got.code, got.acted, len(got.renames), got.stderr)
	}
	binaries.requireUnchanged(t)
	if st := markerOf(t, r.dataDir); st.RollbackStarted {
		t.Errorf("the marker = %+v, want no rollback under way", st)
	}
}

// TestTheCrashRollbackAsksItsBackupBeforeItTakesTheMarkersLock: the backup's
// version and its settings check run with the install marker's lock free, so
// that a run that does not answer within its bound keeps no other taker of
// the lock waiting; the rollback then puts the backup back as it always has,
// its record durable before the rename.
func TestTheCrashRollbackAsksItsBackupBeforeItTakesTheMarkersLock(t *testing.T) {
	r := crashInstallAtTheLimit(t)
	b := &backupRuns{}
	got := crashRollBack(t.Context(), t, r, b)
	want := []string{"the backup's version " + lockFound(false), "the backup's settings check " + lockFound(false)}
	if !slices.Equal(b.asked, want) {
		t.Errorf("the backup was asked %q; want %q", b.asked, want)
	}
	requirePutBack(t, r, got)
}

// refusalOnDisk is a refusal of the crash rollback an earlier start recorded
// in the install marker (State.RollbackRefused), which a start whose
// inspection of the backup fails clears or keeps (backlog B898FU's round 3).
type refusalOnDisk struct {
	name, recorded string
	cleared        bool
}

// refusalsOnDisk are what such a start may find recorded: no refusal; each of
// the words an inspection that failed was recorded in until round 3, a
// lookup's (its round 2) and an open's (backlog B894), which it clears; and a
// refusal of another cause, which it keeps, and with it the auto-installer's
// not waiting (TestTheAutoInstallerDoesNotWaitBesideARefusedRollback). A
// backup that changed while it was checked is such a cause, though a read of
// its identity that failed was recorded in those words too until round 4: they
// record a backup replaced as well, so no start clears them by their words
// (TestTheCrashRollbackPutsBackABackupRecordedAsChangedOnceItsIdentityReads).
var refusalsOnDisk = []refusalOnDisk{
	{name: "no refusal recorded"},
	{name: "a refusal recorded for a lookup that failed", recorded: "the backup could not be inspected", cleared: true},
	{name: "a refusal recorded for an open that failed", recorded: "the backup could not be opened to tell which file its checks ask", cleared: true},
	{name: "a refusal of another cause recorded", recorded: "the backup did not say its version"},
	{name: "a refusal recorded for a backup that changed", recorded: "the backup changed while it was checked"},
}

// lay records d's refusal in r's marker, as an earlier start left it, and
// returns the marker as it reads then and its file, watched.
func (d refusalOnDisk) lay(t *testing.T, r *refusedInstall) (updater.State, watchedFiles) {
	t.Helper()
	r.marker.RollbackRefused = d.recorded
	r.saveMarker(t, r.dataDir)
	return markerOf(t, r.dataDir), watch(t, updater.StatePath(r.dataDir))
}

// requireUnrecorded requires r's marker after a start whose inspection of the
// backup failed, d laid before it (the marker then, before, and its file,
// watched): nothing written where d is kept, and otherwise d's refusal
// cleared and nothing else of the marker changed.
func (d refusalOnDisk) requireUnrecorded(t *testing.T, r *refusedInstall, before updater.State, marker watchedFiles) {
	t.Helper()
	if !d.cleared {
		marker.requireUnchanged(t)
		return
	}
	got := markerOf(t, r.dataDir)
	if got.RollbackRefused != "" {
		t.Errorf("the marker's refusal = %q, want %q, recorded for an inspection that failed, cleared", got.RollbackRefused, d.recorded)
	}
	got.RollbackRefused, before.RollbackRefused = "", ""
	if !reflect.DeepEqual(got, before) {
		t.Errorf("the marker = %+v, want %+v: its refusal alone cleared", got, before)
	}
}

// requireHold requires the auto-installer's hold over the backup beside
// binaryPath, the path the start was given (updater.ProbationHolds, the
// process up a minute), as r's marker reads now: holding where it records no
// refusal, as after a start whose inspection of the backup failed, and not
// where it keeps d's refusal of another cause. when says when it is asked.
func (d refusalOnDisk) requireHold(t *testing.T, r *refusedInstall, binaryPath, when string) {
	t.Helper()
	st := markerOf(t, r.dataDir)
	hold := updater.ProbationHolds(st, st.TargetVersion, binaryPath, time.Minute)
	switch kept := d.recorded != "" && !d.cleared; {
	case !kept && hold == "":
		t.Errorf("%s, the auto-installer does not wait beside a backup the crash rollback could not inspect (the marker's refusal %q)",
			when, st.RollbackRefused)
	case kept && hold != "":
		t.Errorf("%s, the auto-installer waits beside a rollback refused for another cause (%q): %s", when, st.RollbackRefused, hold)
	}
}

// TestTheCrashRollbackTellsABackupItCannotInspectFromNone: a lookup of the
// backup that fails otherwise than finding nothing there (inspectionFailures)
// is refused before the install marker's lock as a backup that could not be
// inspected, never as no backup (backlog B898FU's round 2), and recorded
// nowhere (its round 3): recorded, it lifted the auto-installer's hold
// (updater.ProbationHolds) over the backup, the last version known to run,
// which the hold is for. So the hold stands while the lookup fails, its own
// lookup failing as the rollback's did, and once the lookup has recovered; a
// refusal an earlier start recorded in the words such a failure had is
// cleared, and one of another cause kept (refusalsOnDisk). The backup is
// asked nothing, nothing is put back, and the install stays for the next
// start to ask again.
func TestTheCrashRollbackTellsABackupItCannotInspectFromNone(t *testing.T) {
	for _, f := range inspectionFailures {
		t.Run(f.name, func(t *testing.T) {
			for _, d := range refusalsOnDisk {
				t.Run(d.name, func(t *testing.T) {
					r := crashInstallAtTheLimit(t)
					binaries := watch(t, r.bin, r.bak)
					before, marker := d.lay(t, r)
					binaryPath, recovered := f.laid(t, r.bin)
					given := *r
					given.bin = binaryPath
					b := &backupRuns{}
					got := crashRollBack(t.Context(), t, &given, b)
					d.requireHold(t, r, binaryPath, "while the lookup fails")
					recovered()
					requireLeft(t, r, got, binaries)
					if lines := updaterLines(got.stderr); !strings.Contains(lines, "not rolling back the install") ||
						!strings.Contains(lines, "could not be inspected") || strings.Contains(lines, "there is no backup") {
						t.Errorf("the start did not say that the backup could not be inspected, or said there is none; its updater lines: %q", lines)
					}
					d.requireUnrecorded(t, r, before, marker)
					d.requireHold(t, r, binaryPath, "once the lookup has recovered")
					if len(b.asked) != 0 {
						t.Errorf("the backup was asked %q, want nothing", b.asked)
					}
				})
			}
		})
	}
}

// TestTheCrashRollbackRecordsTheMarkersOwnRefusalBesideABackupItCannotInspect:
// a start whose lookup of the backup fails, a refusal it records nowhere
// (backlog B898FU's round 3), still records the refusal of the marker it reads
// again under the install marker's lock where that marker refuses the rollback
// itself: here a migration of the store, recorded while the start waited for
// the lock by the holder the case stands in for, which let the lock go once it
// had written it, as a migrating store open does. The line says the marker's
// why alone. The lock is held through an open of its lock file of the case's
// own (markersLockHeld), and the holder writes when the start's take first
// finds it held, on the start's goroutine.
func TestTheCrashRollbackRecordsTheMarkersOwnRefusalBesideABackupItCannotInspect(t *testing.T) {
	r := crashInstallAtTheLimit(t)
	binaries := watch(t, r.bin, r.bak)
	binaryPath, recovered := inspectionFailures[0].laid(t, r.bin)
	lock := updater.StateLockPath(r.dataDir)
	held, err := fsutil.OpenLockFile(filepath.Dir(lock), filepath.Base(lock))
	if err != nil {
		t.Fatalf("open the install marker's lock file: %v", err)
	}
	if err := fsutil.TryLock(held); err != nil {
		_ = held.Close()
		t.Fatalf("take the install marker's lock: %v", err)
	}
	t.Cleanup(func() {
		_ = fsutil.Unlock(held)
		_ = held.Close()
	})
	migrated := false
	t.Cleanup(updater.SetStateLockWaitingForTest(func(dataDir string) {
		if dataDir != r.dataDir || migrated {
			return
		}
		migrated = true
		st := markerOf(t, r.dataDir)
		st.StoreMigratedBy = version.ServerVersion
		if err := updater.SaveState(r.dataDir, st); err != nil {
			t.Errorf("record the migration: %v", err)
		}
		if err := fsutil.Unlock(held); err != nil {
			t.Errorf("let the install marker's lock go: %v", err)
		}
	}))
	given := *r
	given.bin = binaryPath
	got := crashRollBack(t.Context(), t, &given, &backupRuns{})
	recovered()
	if !migrated {
		t.Fatalf("the start never waited for the lock held for the case: the test shows nothing; stderr=%s", got.stderr)
	}
	requireLeft(t, r, got, binaries)
	requireRefusal(t, r, got.stderr, "began a migration of the store", "began a migration of the store")
	if lines := updaterLines(got.stderr); strings.Contains(lines, "could not be inspected") {
		t.Errorf("the start said the lookup's refusal beside the marker's own; its updater lines: %q", lines)
	}
}

// TestTheCrashRollbackTellsABackupItCannotOpenFromNone: a backup whose lookup
// finds a regular file that this user may not open, its read permission
// taken off, is refused before the install marker's lock in the open's own
// words, as a backup that could not be opened to tell which file its checks
// ask, and recorded nowhere (backlog B898FU's round 3), as a lookup that
// fails is: the auto-installer's hold stands while the open fails, its own
// lookup finding a regular file there, and once the backup opens again; a
// refusal an earlier start recorded in the words such a failure had is
// cleared, and one of another cause kept (refusalsOnDisk). The backup is
// asked nothing and nothing is put back. A file this user may not open needs
// POSIX modes and a user that is not root, whom no mode refuses.
func TestTheCrashRollbackTellsABackupItCannotOpenFromNone(t *testing.T) {
	for _, d := range refusalsOnDisk {
		t.Run(d.name, func(t *testing.T) {
			if runtime.GOOS == "windows" || os.Geteuid() == 0 {
				t.Skip("a file this user may not open needs POSIX modes and a user that is not root")
			}
			r := crashInstallAtTheLimit(t)
			binaries := watch(t, r.bin, r.bak)
			before, marker := d.lay(t, r)
			fi, err := os.Lstat(r.bak)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(r.bak, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(r.bak, fi.Mode().Perm()) })
			if now, err := os.Lstat(r.bak); err != nil || !now.Mode().IsRegular() {
				t.Fatalf("the premise: a lookup of the backup answered %v (%v), want a regular file", now, err)
			}
			if f, _, err := fsutil.OpenAsFile(r.bak); !errors.Is(err, fs.ErrPermission) {
				if f != nil {
					_ = f.Close()
				}
				t.Fatalf("the premise: an open of the backup answered %v, want %v", err, fs.ErrPermission)
			}
			b := &backupRuns{}
			got := crashRollBack(t.Context(), t, r, b)
			d.requireHold(t, r, r.bin, "while the backup cannot be opened")
			if err := os.Chmod(r.bak, fi.Mode().Perm()); err != nil {
				t.Fatal(err)
			}
			requireLeft(t, r, got, binaries)
			if lines := updaterLines(got.stderr); !strings.Contains(lines, "not rolling back the install") ||
				!strings.Contains(lines, "could not be opened to tell which file its checks ask") {
				t.Errorf("the start did not say that the backup could not be opened; its updater lines: %q", lines)
			}
			d.requireUnrecorded(t, r, before, marker)
			d.requireHold(t, r, r.bin, "once the backup opens again")
			if len(b.asked) != 0 {
				t.Errorf("the backup was asked %q, want nothing", b.asked)
			}
		})
	}
}

// TestTheCrashRollbackTellsABackupItCannotInspectUnderItsLockFromAChange: a
// backup whose lookup comes to fail otherwise than finding nothing there
// while its settings check runs, the directory it is in made one this user
// may not search or a link on the way to it made a loop, is found so by the
// lookup the rollback asks again under the install marker's lock, said as a
// backup that could not be inspected again, never as one that changed, and
// recorded nowhere (backlog B898FU's round 3; until then it was recorded as a
// backup that changed while it was checked, which lifted the auto-installer's
// hold): the hold stands while the lookup fails and once it has recovered; a
// refusal an earlier start recorded in the words such a failure had is
// cleared, and one of another cause kept (refusalsOnDisk). Nothing is put
// back. A backup that went while it was checked is a change still
// (TestABackupChangedWhileItWasCheckedIsNotPutBack).
func TestTheCrashRollbackTellsABackupItCannotInspectUnderItsLockFromAChange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		// path is the binary's path the rollback is given, which reaches the
		// binary and its backup until lay makes the backup's lookup fail;
		// recovered, which lay returns, lets it reach them again.
		path func(t *testing.T, r *refusedInstall) string
		lay  func(t *testing.T, r *refusedInstall) (recovered func())
	}{
		{"a directory this user may not search", fs.ErrPermission,
			func(t *testing.T, r *refusedInstall) string {
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("a directory this user may not search needs POSIX modes and a user that is not root")
				}
				return r.bin
			},
			func(t *testing.T, r *refusedInstall) func() {
				dir := filepath.Dir(r.bin)
				fi, err := os.Stat(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, fi.Mode().Perm()) })
				return func() {
					if err := os.Chmod(dir, fi.Mode().Perm()); err != nil {
						t.Fatal(err)
					}
				}
			}},
		{"a link loop on the way", syscall.ELOOP,
			func(t *testing.T, r *refusedInstall) string {
				if runtime.GOOS == "windows" {
					t.Skip("a link loop needs symbolic links, which Windows gives only with a privilege")
				}
				via := filepath.Join(r.dir, "via")
				if err := os.Symlink(filepath.Dir(r.bin), via); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(via, filepath.Base(r.bin))
			},
			func(t *testing.T, r *refusedInstall) func() {
				via := filepath.Join(r.dir, "via")
				point := func(target string) {
					if err := os.Remove(via); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, via); err != nil {
						t.Fatal(err)
					}
				}
				point("via")
				return func() { point(filepath.Dir(r.bin)) }
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, d := range refusalsOnDisk {
				t.Run(d.name, func(t *testing.T) {
					r := crashInstallAtTheLimit(t)
					binaryPath := tc.path(t, r)
					binaries := watch(t, r.bin, r.bak)
					before, marker := d.lay(t, r)
					given := *r
					given.bin = binaryPath
					recovered := func() {}
					b := &backupRuns{check: func(context.Context) updater.ConfigCheck {
						recovered = tc.lay(t, r)
						if _, err := os.Lstat(binaryPath + ".bak"); !errors.Is(err, tc.cause) {
							recovered()
							t.Fatalf("the premise: a lookup of the backup during its settings check answered %v, want %v", err, tc.cause)
						}
						return updater.ConfigCheck{Verdict: updater.ConfigValid}
					}}
					got := crashRollBack(t.Context(), t, &given, b)
					d.requireHold(t, r, binaryPath, "while the lookup fails")
					recovered()
					requireLeft(t, r, got, binaries)
					if lines := updaterLines(got.stderr); !strings.Contains(lines, "not rolling back the install") ||
						!strings.Contains(lines, "could not be inspected again") || strings.Contains(lines, "the backup changed while it was checked") {
						t.Errorf("the start did not say that the backup could not be inspected again, or said it changed; its updater lines: %q", lines)
					}
					d.requireUnrecorded(t, r, before, marker)
					d.requireHold(t, r, binaryPath, "once the lookup has recovered")
					want := []string{"the backup's version " + lockFound(false), "the backup's settings check " + lockFound(false)}
					if !slices.Equal(b.asked, want) {
						t.Errorf("the backup was asked %q; want %q", b.asked, want)
					}
				})
			}
		})
	}
}

// errIdentityNotRead is what a case's read of the backup's identity answers
// where the case fails it.
var errIdentityNotRead = errors.New("its identity could not be read")

// TestTheCrashRollbackTellsABackupWhoseIdentityCannotBeReadFromAChange: an
// unchanged backup whose identity cannot be read, before the install
// marker's lock or under it (serveOpts.backupIdentity failing that read, as a
// Windows read of a file's ID may fail while the file stands as it was), is
// said as a backup that could not be inspected, never as one that changed,
// and recorded nowhere (backlog B898FU's round 4: until then such a read
// compared as another file, recorded "the backup changed while it was
// checked", and so ended the auto-installer's hold): the hold stands; a
// refusal an earlier start recorded in the words an inspection that failed
// had is cleared, and one of any other cause kept, a backup that changed among
// them (refusalsOnDisk). Nothing is put back, and the read that fails is the
// case's: the first, before the lock and before the runs, or the second,
// under it, the runs asked.
func TestTheCrashRollbackTellsABackupWhoseIdentityCannotBeReadFromAChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		failed int // the read that fails
		said   string
		runs   int
	}{
		{"before the install marker's lock", 1, "could not be inspected (", 0},
		{"under the install marker's lock", 2, "could not be inspected again", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, d := range refusalsOnDisk {
				t.Run(d.name, func(t *testing.T) {
					r := crashInstallAtTheLimit(t)
					binaries := watch(t, r.bin, r.bak)
					before, marker := d.lay(t, r)
					reads := 0
					b := &backupRuns{identity: func(path string) (os.FileInfo, error) {
						reads++
						if reads == tc.failed {
							return nil, &fs.PathError{Op: "read the identity of", Path: path, Err: errIdentityNotRead}
						}
						return fsutil.EntryIdentity(path)
					}}
					got := crashRollBack(t.Context(), t, r, b)
					requireLeft(t, r, got, binaries)
					if lines := updaterLines(got.stderr); !strings.Contains(lines, "not rolling back the install") ||
						!strings.Contains(lines, tc.said) || strings.Contains(lines, "the backup changed while it was checked") {
						t.Errorf("the start did not say that the backup could not be inspected, or said it changed; its updater lines: %q", lines)
					}
					if reads != tc.failed || len(b.asked) != tc.runs {
						t.Errorf("the identity was read %d times and the backup asked %q; want %d reads and %d runs", reads, b.asked, tc.failed, tc.runs)
					}
					d.requireUnrecorded(t, r, before, marker)
					d.requireHold(t, r, r.bin, "once the start goes on")
				})
			}
		})
	}
}

// TestTheCrashRollbackPutsBackABackupAnotherHandleReads: a backup another
// handle holds open for reading, sharing reading and writing as os.Open's
// does (an indexer or a scanner reading it), is inspected and compared as one
// nobody holds, and put back, the reader let go just before the put-back,
// which a handle that shares no deletion refuses on Windows; the
// auto-installer's hold stands while the reader holds it. The case is
// Windows' (backlog B898FU's round 4): there os.SameFile read an Lstat's
// identity by the path as it compared, opening the file with no access asked
// for and nothing shared, beside the reader's handle; elsewhere a reader
// changes nothing.
func TestTheCrashRollbackPutsBackABackupAnotherHandleReads(t *testing.T) {
	r := crashInstallAtTheLimit(t)
	reader, err := os.Open(r.bak)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	letGo := func() {
		if held {
			held = false
			_ = reader.Close()
		}
	}
	t.Cleanup(letGo)
	b := &backupRuns{check: func(context.Context) updater.ConfigCheck {
		st := markerOf(t, r.dataDir)
		if updater.ProbationHolds(st, st.TargetVersion, r.bin, time.Minute) == "" {
			t.Error("while another handle reads the backup, the auto-installer does not wait")
		}
		return updater.ConfigCheck{Verdict: updater.ConfigValid}
	}}
	var got crashRollback
	restore := func(dst, ext string) error {
		got.renames = append(got.renames, markerOf(t, r.dataDir))
		letGo()
		return updater.RollbackBinary(dst, ext)
	}
	var stderr bytes.Buffer
	got.code, got.acted = recoverCrashedInstall(t.Context(), r.dataDir, r.cfg, b.opts(t, r, restore), crashRollbackGoesOn, &stderr)
	got.stderr = stderr.String()
	requirePutBack(t, r, got)
}

// TestTheCrashRollbackTellsALinkAtItsBackupFromABackupItCannotInspect: a link
// at the backup's path is read as the entry it is, never followed, and is no
// regular file: the rollback refuses it as no backup to put back, said and
// recorded, as it refuses any entry that is no regular file, never as a
// backup that could not be inspected, which only a read that fails is (backlog
// B898FU's round 4: on Windows the entry is read through a handle that opens a
// reparse point itself). The backup is asked nothing, and the auto-installer
// does not wait beside the refusal. A symbolic link to a regular file, which
// Windows makes only with a privilege, and a link to a directory, there a
// directory junction (linkCacheDirOrSkip).
func TestTheCrashRollbackTellsALinkAtItsBackupFromABackupItCannotInspect(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(t *testing.T, r *refusedInstall)
	}{
		{"a symbolic link to a regular file", func(t *testing.T, r *refusedInstall) {
			target := filepath.Join(r.dir, "a file elsewhere")
			if err := os.Rename(r.bak, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, r.bak); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("this host makes no symbolic link without a privilege: %v", err)
				}
				t.Fatal(err)
			}
		}},
		{"a link to a directory, a junction on Windows", func(t *testing.T, r *refusedInstall) {
			target := filepath.Join(r.dir, "a directory elsewhere")
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(r.bak); err != nil {
				t.Fatal(err)
			}
			linkCacheDirOrSkip(t, target, r.bak)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := crashInstallAtTheLimit(t)
			tc.link(t, r)
			if fi, err := os.Lstat(r.bak); err != nil || fi.Mode().IsRegular() {
				t.Fatalf("the premise: a lookup of the link answered %v (%v), want an entry that is no regular file", fi, err)
			}
			binaries := watch(t, r.bin)
			b := &backupRuns{}
			got := crashRollBack(t.Context(), t, r, b)
			requireLeft(t, r, got, binaries)
			requireRefusal(t, r, got.stderr, "there is no backup to put back", "there is no backup beside the binary to put back")
			if lines := updaterLines(got.stderr); strings.Contains(lines, "could not be inspected") {
				t.Errorf("the start said a link at the backup's path could not be inspected; its updater lines: %q", lines)
			}
			if len(b.asked) != 0 {
				t.Errorf("the backup was asked %q, want nothing", b.asked)
			}
			st := markerOf(t, r.dataDir)
			if why := updater.ProbationHolds(st, st.TargetVersion, r.bin, time.Minute); why != "" {
				t.Errorf("beside a link refused at the backup's path, the auto-installer waits: %s", why)
			}
		})
	}
}

// TestTheCrashRollbackPutsBackABackupRecordedAsChangedOnceItsIdentityReads: a
// refusal recorded as a backup that changed while it was checked, which a
// read of the backup's identity that failed wrote until backlog B898FU's round
// 4, as a backup replaced writes still, is cleared by no start for its words;
// the next start inspects the backup again, and where each identity reads and
// they match, goes on and puts the backup back, its record in that refusal's
// place (the recovery that round chose). While the read keeps failing, the
// refusal stands and the auto-installer does not wait (refusalsOnDisk's row of
// a backup that changed).
func TestTheCrashRollbackPutsBackABackupRecordedAsChangedOnceItsIdentityReads(t *testing.T) {
	r := crashInstallAtTheLimit(t)
	r.marker.RollbackRefused = "the backup changed while it was checked"
	r.saveMarker(t, r.dataDir)
	got := crashRollBack(t.Context(), t, r, &backupRuns{})
	requirePutBack(t, r, got)
	if at := got.renames[0]; at.RollbackRefused != "" {
		t.Errorf("the marker as the backup was put back records %q, want the refusal gone with the rollback's record", at.RollbackRefused)
	}
}

// handleIdentity is path's stat taken through a handle on it, closed at once:
// on Windows an Lstat's file ID is read by path when os.SameFile first
// compares it, so only a handle's stat fixes which file it is as it is taken.
func handleIdentity(t *testing.T, path string) os.FileInfo {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// replacementBeside is a file of its own beside r's backup, holding the
// backup's bytes and given its modification time: a file no size, no time and
// no answer of its runs tells from the backup, but its identity, which it
// requires to differ before any case changes anything.
func replacementBeside(t *testing.T, r *refusedInstall) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(r.bak), "another backup")
	if err := os.WriteFile(path, []byte(backupBytes), 0o755); err != nil {
		t.Fatal(err)
	}
	bak := handleIdentity(t, r.bak)
	if err := os.Chtimes(path, bak.ModTime(), bak.ModTime()); err != nil {
		t.Fatal(err)
	}
	other := handleIdentity(t, path)
	if os.SameFile(bak, other) || other.Size() != bak.Size() || !other.ModTime().Equal(bak.ModTime()) {
		t.Fatalf("the replacement (%d bytes, %v) beside the backup (%d bytes, %v), the same file %v: "+
			"want two files of one size and one time, so that only their identity tells them apart",
			other.Size(), other.ModTime(), bak.Size(), bak.ModTime(), os.SameFile(bak, other))
	}
	return path
}

// TestABackupChangedWhileItWasCheckedIsNotPutBack: the backup's identity is
// fixed before its runs and asked again under the install marker's lock, so a
// backup replaced while it was asked, in its settings check or between its
// version and its check, by a file that no size, time or answer tells from it,
// is not put back, nor one rewritten in place to another size, its time kept,
// or whose modification time alone moved, nor one gone while it was checked,
// a lookup that finds nothing there being a change, where one that fails
// otherwise is not (backlog B898FU's round 3,
// TestTheCrashRollbackTellsABackupItCannotInspectUnderItsLockFromAChange):
// the start says so and records it, and the next start of the loop asks
// again.
func TestABackupChangedWhileItWasCheckedIsNotPutBack(t *testing.T) {
	const why = "the backup changed while it was checked"
	replace := func(t *testing.T, r *refusedInstall, replacement string) {
		if err := os.Rename(replacement, r.bak); err != nil {
			t.Fatal(err)
		}
	}
	rewrite := func(t *testing.T, r *refusedInstall, _ string) {
		before := handleIdentity(t, r.bak)
		f, err := os.OpenFile(r.bak, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(", rewritten"); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(r.bak, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
		after := handleIdentity(t, r.bak)
		if !os.SameFile(before, after) || after.Size() == before.Size() || !after.ModTime().Equal(before.ModTime()) {
			t.Fatalf("the rewrite left %d bytes at %v, the same file %v: want the same file, another size and the same time",
				after.Size(), after.ModTime(), os.SameFile(before, after))
		}
	}
	touch := func(t *testing.T, r *refusedInstall, _ string) {
		before := handleIdentity(t, r.bak)
		moved := before.ModTime().Add(-time.Hour)
		if err := os.Chtimes(r.bak, moved, moved); err != nil {
			t.Fatal(err)
		}
		after := handleIdentity(t, r.bak)
		if !os.SameFile(before, after) || after.Size() != before.Size() || after.ModTime().Equal(before.ModTime()) {
			t.Fatalf("the touch left %d bytes at %v, the same file %v: want the same file and size, another time",
				after.Size(), after.ModTime(), os.SameFile(before, after))
		}
	}
	remove := func(t *testing.T, r *refusedInstall, _ string) {
		if err := os.Remove(r.bak); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		inThe  string
		change func(t *testing.T, r *refusedInstall, replacement string)
		gone   bool
	}{
		{"replaced during its settings check", "check", replace, false},
		{"replaced between its version and its check", "version", replace, false},
		{"rewritten to another size, its time kept", "check", rewrite, false},
		{"its modification time alone moved", "check", touch, false},
		{"gone during its settings check", "check", remove, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := crashInstallAtTheLimit(t)
			replacement := replacementBeside(t, r)
			binaries := watch(t, r.bin)
			b := &backupRuns{}
			switch tc.inThe {
			case "version":
				b.version = func(context.Context) (string, error) {
					tc.change(t, r, replacement)
					return previousVersion, nil
				}
			case "check":
				b.check = func(context.Context) updater.ConfigCheck {
					tc.change(t, r, replacement)
					return updater.ConfigCheck{Verdict: updater.ConfigValid}
				}
			}
			got := crashRollBack(t.Context(), t, r, b)
			requireLeft(t, r, got, binaries)
			requireRefusal(t, r, got.stderr, why, why)
			if !tc.gone {
				requireBackupThere(t, r)
			}
		})
	}
}

// TestTheBackupIsNotPutBackOverSettingsItDidNotCheck: the settings' digest is
// taken before the backup's runs and again under the install marker's lock,
// each from a read that must succeed: settings changed while the backup
// checked them, gone while it checked them, or that could not be read before
// its runs are not the settings its check answered for, and the backup is not
// put back over them. Each is said and recorded in words that name no path,
// and settings that cannot be read are refused before anything runs.
func TestTheBackupIsNotPutBackOverSettingsItDidNotCheck(t *testing.T) {
	appendAComment := func(t *testing.T, r *refusedInstall) {
		f, err := os.OpenFile(r.cfg, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("# a comment the backup's check never read\n"); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(t *testing.T, r *refusedInstall) {
		if err := os.Remove(r.cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name           string
		before, during func(t *testing.T, r *refusedInstall)
		why            string
		runs           int
	}{
		{"a comment appended while it checked them", nil, appendAComment, "the settings changed while the backup checked them", 2},
		{"gone while it checked them", nil, remove, "the settings could not be read again after the backup's settings check", 2},
		{"unreadable before its runs", remove, nil, "the settings could not be read before the backup's settings check", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := crashInstallAtTheLimit(t)
			binaries := watch(t, r.bin, r.bak)
			b := &backupRuns{}
			if tc.during != nil {
				b.check = func(context.Context) updater.ConfigCheck {
					tc.during(t, r)
					return updater.ConfigCheck{Verdict: updater.ConfigValid}
				}
			}
			if tc.before != nil {
				tc.before(t, r)
			}
			got := crashRollBack(t.Context(), t, r, b)
			requireLeft(t, r, got, binaries)
			requireRefusal(t, r, got.stderr, tc.why, tc.why)
			if strings.Contains(updaterLines(got.stderr), r.cfg) {
				t.Errorf("the start's line names the settings' path; its updater lines: %q", updaterLines(got.stderr))
			}
			if len(b.asked) != tc.runs {
				t.Errorf("the backup was asked %q, want %d runs", b.asked, tc.runs)
			}
		})
	}
}

// TestTheCrashRollbackDecidesAgainOnTheMarkerItReadsUnderTheLock: the marker
// is read again once the install marker's lock is held, after the backup's
// runs, and the rollback acts only on what it reads then. Another install's
// marker, saved while the backup was checked, is left as it was written,
// nothing said, recorded or renamed over it. A marker of the install's target
// and attempt that names another version as the one it replaced is refused in
// the words the backup's version has always been refused in, its answer held
// to the marker read again. And an install made again meanwhile, its versions
// and its count as they were and its AttemptedAt alone moved, is refused as
// another install than the one the backup was checked for.
func TestTheCrashRollbackDecidesAgainOnTheMarkerItReadsUnderTheLock(t *testing.T) {
	t.Run("another install's marker", func(t *testing.T) {
		r := crashInstallAtTheLimit(t)
		binaries := watch(t, r.bin, r.bak)
		var written watchedFiles
		b := &backupRuns{check: func(context.Context) updater.ConfigCheck {
			another := updater.State{Status: "installing", TargetVersion: "9.9.9", PreviousVersion: version.ServerVersion,
				AttemptedAt: time.Now().UTC(), SwapStarted: true}
			if err := updater.SaveState(r.dataDir, another); err != nil {
				t.Fatal(err)
			}
			written = watch(t, updater.StatePath(r.dataDir))
			return updater.ConfigCheck{Verdict: updater.ConfigValid}
		}}
		got := crashRollBack(t.Context(), t, r, b)
		requireLeft(t, r, got, binaries)
		if written == nil {
			t.Fatal("the backup's settings check was never asked: the test shows nothing")
		}
		written.requireUnchanged(t)
		if lines := updaterLines(got.stderr); lines != "" {
			t.Errorf("the crash rollback spoke over another install's marker: %q", lines)
		}
	})
	for _, tc := range []struct {
		name           string
		change         func(st *updater.State)
		said, recorded string
	}{
		{"another version replaced", func(st *updater.State) { st.PreviousVersion = "0.0.0-other" },
			"is " + previousVersion + ", where the install replaced 0.0.0-other",
			"the backup is " + previousVersion + ", where the install replaced 0.0.0-other"},
		{"the install made again", func(st *updater.State) { st.AttemptedAt = st.AttemptedAt.Add(time.Second) },
			"the install changed while its backup was checked", "the install changed while its backup was checked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := crashInstallAtTheLimit(t)
			binaries := watch(t, r.bin, r.bak)
			b := &backupRuns{check: func(context.Context) updater.ConfigCheck {
				st := markerOf(t, r.dataDir)
				tc.change(&st)
				if ok, why := updater.CrashRollback(st, version.ServerVersion); !ok {
					t.Fatalf("the marker changed to %+v asks for no crash rollback (%q): the test shows nothing", st, why)
				}
				if err := updater.SaveState(r.dataDir, st); err != nil {
					t.Fatal(err)
				}
				return updater.ConfigCheck{Verdict: updater.ConfigValid}
			}}
			got := crashRollBack(t.Context(), t, r, b)
			requireLeft(t, r, got, binaries)
			requireRefusal(t, r, got.stderr, tc.said, tc.recorded)
		})
	}
}

// TestAStopAroundTheBackupsChecksWritesNothing: serve's stop while the
// backup's runs run, or as they answer (the install marker's lock taken with a
// context that has ended, which a take that needs no wait does), writes
// nothing: the install marker and the binaries as they were. A stop in the
// version's run is said as that refusal has always been said, one in the
// settings check as it was before, and one that came as the check answered
// once the lock is held, before anything is recorded or renamed.
func TestAStopAroundTheBackupsChecksWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs func(stop context.CancelFunc) *backupRuns
		said string
	}{
		{"in the backup's version", func(stop context.CancelFunc) *backupRuns {
			return &backupRuns{version: func(ctx context.Context) (string, error) {
				stop()
				return "", ctx.Err()
			}}
		}, "did not say its version: " + context.Canceled.Error()},
		{"in the backup's settings check", func(stop context.CancelFunc) *backupRuns {
			return &backupRuns{check: func(ctx context.Context) updater.ConfigCheck {
				stop()
				return updater.ConfigCheck{Verdict: updater.ConfigCheckFailed, Detail: "the check was stopped", Err: ctx.Err()}
			}}
		}, "this start was stopped while the backup checked the settings"},
		{"as the settings check answered", func(stop context.CancelFunc) *backupRuns {
			return &backupRuns{check: func(context.Context) updater.ConfigCheck {
				stop()
				return updater.ConfigCheck{Verdict: updater.ConfigValid}
			}}
		}, "this start was stopped before it put the backup back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := crashInstallAtTheLimit(t)
			files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			got := crashRollBack(ctx, t, r, tc.runs(stop))
			if got.code != 0 || got.acted || len(got.renames) != 0 {
				t.Errorf("the stopped crash rollback = %d (acted %v) after %d renames, want it to leave the install; stderr=%s",
					got.code, got.acted, len(got.renames), got.stderr)
			}
			if lines := updaterLines(got.stderr); !strings.Contains(lines, "not rolling back the install") || !strings.Contains(lines, tc.said) {
				t.Errorf("the stopped start did not say %q; its updater lines: %q", tc.said, lines)
			}
			files.requireUnchanged(t)
		})
	}
}

// TestTheCrashRollbackFollowsAnInstallMadeWhileItsBackupWasChecked: the
// backup's runs no longer hold the install marker's lock, so an install
// (`bridge update`) can take it while they run. One that lands, its marker of
// another version saved and its swap made (the binary it replaced moved to the
// backup's place, its candidate put at the binary's), is left as it landed:
// nothing said, recorded or renamed over it. One whose swap was refused, as
// Windows refuses the vacate onto a backup a run still has mapped
// (internal/updater's TestARenameOntoTheRunningImageIsAnsweredAtOnce), its
// marker saved and put back as it read it and nothing moved, leaves the
// install as the backup's runs found it, and the rollback goes on.
func TestTheCrashRollbackFollowsAnInstallMadeWhileItsBackupWasChecked(t *testing.T) {
	install := updater.State{Status: "installing", TargetVersion: "9.9.9", PreviousVersion: version.ServerVersion,
		AttemptedAt: time.Now().UTC(), SwapStarted: true}
	t.Run("the install lands", func(t *testing.T) {
		r := crashInstallAtTheLimit(t)
		candidate := filepath.Join(filepath.Dir(r.bin), "candidate")
		if err := os.WriteFile(candidate, []byte("the install's candidate"), 0o755); err != nil {
			t.Fatal(err)
		}
		replaced, err := os.ReadFile(r.bin)
		if err != nil {
			t.Fatal(err)
		}
		var landed watchedFiles
		b := &backupRuns{check: func(context.Context) updater.ConfigCheck {
			if err := updater.SaveState(r.dataDir, install); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(r.bin, r.bak); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(candidate, r.bin); err != nil {
				t.Fatal(err)
			}
			landed = watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
			return updater.ConfigCheck{Verdict: updater.ConfigValid}
		}}
		got := crashRollBack(t.Context(), t, r, b)
		if got.code != 0 || got.acted || len(got.renames) != 0 {
			t.Errorf("the crash rollback over the install that landed = %d (acted %v) after %d renames, want it to leave it; stderr=%s",
				got.code, got.acted, len(got.renames), got.stderr)
		}
		if landed == nil {
			t.Fatal("the backup's settings check was never asked: the test shows nothing")
		}
		landed.requireUnchanged(t)
		if b, err := os.ReadFile(r.bak); err != nil || !bytes.Equal(b, replaced) {
			t.Errorf("the backup after the install = %q (%v), want the binary the install replaced", b, err)
		}
		if lines := updaterLines(got.stderr); lines != "" {
			t.Errorf("the crash rollback spoke over the install that landed: %q", lines)
		}
	})
	t.Run("the install's swap is refused", func(t *testing.T) {
		r := crashInstallAtTheLimit(t)
		b := &backupRuns{check: func(context.Context) updater.ConfigCheck {
			read := markerOf(t, r.dataDir)
			if err := updater.SaveState(r.dataDir, install); err != nil {
				t.Fatal(err)
			}
			if err := updater.SaveState(r.dataDir, read); err != nil {
				t.Fatal(err)
			}
			return updater.ConfigCheck{Verdict: updater.ConfigValid}
		}}
		requirePutBack(t, r, crashRollBack(t.Context(), t, r, b))
	})
}

// TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion: a
// refusal decided before the install marker's lock is said and recorded under
// it only where the marker read again is the install the refusal was decided
// for, its TargetVersion, its PreviousVersion and its AttemptedAt
// (refuseCrashRollbackBeforeTheLock). A marker that names another version as
// the one the install replaced, its target, its attempt and its count as they
// were and the crash rollback still asked for, is another install's: the
// refusal, decided of the install the marker no longer describes, is neither
// said nor recorded over it, whichever of the backup's runs refused, and the
// next start asks again. Each run's stand-in saves that marker, then answers
// its refusal.
func TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		runs func(replaced func()) *backupRuns
	}{
		{"the backup's version did not answer", func(replaced func()) *backupRuns {
			return &backupRuns{version: func(context.Context) (string, error) {
				replaced()
				return "", errors.New("the version's run ended with no answer")
			}}
		}},
		{"the backup refused the settings", func(replaced func()) *backupRuns {
			return &backupRuns{check: func(context.Context) updater.ConfigCheck {
				replaced()
				return updater.ConfigCheck{Verdict: updater.ConfigRefused, Reasons: []string{"line 3: a key the backup does not know"}}
			}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := crashInstallAtTheLimit(t)
			binaries := watch(t, r.bin, r.bak)
			var written watchedFiles
			replaced := func() {
				st := markerOf(t, r.dataDir)
				st.PreviousVersion = "0.0.0-other"
				if ok, why := updater.CrashRollback(st, version.ServerVersion); !ok {
					t.Fatalf("the marker changed to %+v asks for no crash rollback (%q): the test shows nothing", st, why)
				}
				if err := updater.SaveState(r.dataDir, st); err != nil {
					t.Fatal(err)
				}
				written = watch(t, updater.StatePath(r.dataDir))
			}
			got := crashRollBack(t.Context(), t, r, tc.runs(replaced))
			requireLeft(t, r, got, binaries)
			if written == nil {
				t.Fatal("no run of the backup saved the marker: the test shows nothing")
			}
			written.requireUnchanged(t)
			if lines := updaterLines(got.stderr); lines != "" {
				t.Errorf("the crash rollback spoke over a marker naming another replaced version: %q", lines)
			}
			if st := markerOf(t, r.dataDir); st.RollbackRefused != "" {
				t.Errorf("the marker's refusal = %q, want none recorded over another install", st.RollbackRefused)
			}
		})
	}
}

// TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld: a
// refusal decided before the install marker's lock, where another holder keeps
// that lock for the whole of LockState's wait (an install between its decision
// and its swap, say), is said from the marker checked and recorded nowhere,
// since only the lock's holder writes the marker: the install marker and both
// binaries are left byte for byte as they were. The case holds the lock
// through an open of the lock file of its own, which a take in this process
// waits on as on another process's (markersLockHeld), with the wait
// shortened and serve's context live, so the helper's take ends at its wait's
// end with LockState's *updater.StateLockError, never at a stop: the same take
// answers that error before the rollback runs, the rollback's take is heard
// waiting on the held lock, the context has not ended once it returns, and the
// case's own take shows the filesystem keeps locks.
func TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld(t *testing.T) {
	r := crashInstallAtTheLimit(t)
	lock := updater.StateLockPath(r.dataDir)
	f, err := fsutil.OpenLockFile(filepath.Dir(lock), filepath.Base(lock))
	if err != nil {
		t.Fatalf("open the install marker's lock file: %v", err)
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		t.Fatalf("take the install marker's lock: %v", err)
	}
	t.Cleanup(func() {
		_ = fsutil.Unlock(f)
		_ = f.Close()
	})
	t.Cleanup(updater.SetStateLockWaitForTest(100 * time.Millisecond))
	waits := 0
	t.Cleanup(updater.SetStateLockWaitingForTest(func(dataDir string) {
		if dataDir == r.dataDir {
			waits++
		}
	}))
	ctx := t.Context()
	var refused *updater.StateLockError
	if l, err := updater.LockState(ctx, r.dataDir); !errors.As(err, &refused) || !errors.Is(err, updater.ErrStateLockHeld) ||
		ctx.Err() != nil || waits != 1 {
		l.Release()
		t.Fatalf("the install marker's lock, held for the case, answered %+v, %v after %d waits (the context's end: %v); "+
			"want a *updater.StateLockError of a lock held for the whole wait, after one, the context live", l, err, waits, ctx.Err())
	}
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	const reason = "line 3: a key the backup does not know"
	b := &backupRuns{check: func(context.Context) updater.ConfigCheck {
		return updater.ConfigCheck{Verdict: updater.ConfigRefused, Reasons: []string{reason}}
	}}
	got := crashRollBack(ctx, t, r, b)
	want := []string{"the backup's version " + lockFound(true), "the backup's settings check " + lockFound(true)}
	if !slices.Equal(b.asked, want) {
		t.Errorf("the backup was asked %q; want %q, the case's hold standing", b.asked, want)
	}
	if got.code != 0 || got.acted || len(got.renames) != 0 {
		t.Errorf("the crash rollback = %d (acted %v) after %d renames, want it to leave the install; stderr=%s",
			got.code, got.acted, len(got.renames), got.stderr)
	}
	const said = "refuses this bridge's settings, so it would not start on them: " + reason
	if lines := updaterLines(got.stderr); !strings.Contains(lines, "not rolling back the install") || !strings.Contains(lines, said) {
		t.Errorf("the start did not say %q; its updater lines: %q", said, lines)
	}
	files.requireUnchanged(t)
	if waits != 2 || ctx.Err() != nil {
		t.Errorf("the rollback's take of the held lock was heard waiting %d times (the context's end: %v); want once, the context live",
			waits-1, ctx.Err())
	}
}

// TestTheCrashRollbackFixesTheBackupsIdentityThroughAHandle: the identity the
// crash rollback holds its backup to under the install marker's lock is the
// stat of a handle opened on the backup before its runs (fsutil.OpenAsFile's
// stat of the file it opened), and each observation of the backup's entry it
// is compared with has its identity read as it is taken: through the reader
// recoverCrashedInstall falls back to fsutil.EntryIdentity for (a test's
// serveOpts.backupIdentity otherwise), never a bare Lstat's or Stat's, which
// it calls nowhere (backlog B898FU's round 4). What rests on it is Windows'
// alone: there an Lstat's file ID is read by path when os.SameFile first
// compares it (Go 1.27.3's os/types_windows.go, sameFile and loadFileId), so
// an Lstat taken before the runs and compared under the lock would take a
// file put in the backup's place meanwhile for the backup, and a read that
// fails there compares as another file, where elsewhere an Lstat carries its
// device and inode as it is taken. So the source is read, on every host: the
// second result of recoverCrashedInstall's one fsutil.OpenAsFile is the
// identity, which nothing else there binds, and every os.SameFile there
// compares it with a name a call of that reader binds, one of them after the
// install marker's lock is taken. What the reads do on Windows the behavioural
// cases carry: TestTheCrashRollbackTellsABackupWhoseIdentityCannotBeReadFromAChange,
// TestTheCrashRollbackPutsBackABackupAnotherHandleReads and fsutil's cases of
// EntryIdentity.
func TestTheCrashRollbackFixesTheBackupsIdentityThroughAHandle(t *testing.T) {
	const name = "serve_crashed_install.go"
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, strings.ReplaceAll(string(raw), "\r\n", "\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if d, ok := d.(*ast.FuncDecl); ok && d.Recv == nil && d.Name.Name == "recoverCrashedInstall" && d.Body != nil {
			fn = d
		}
	}
	if fn == nil {
		t.Fatalf("%s declares no recoverCrashedInstall", name)
	}
	calls := func(n ast.Node, pkg, function string) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != function {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == pkg
	}
	selects := func(e ast.Expr, pkg, name string) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == pkg
	}
	identity, reader, opens, lockAt := "", "", 0, token.NoPos
	var sameFiles, bareStats []*ast.CallExpr
	var reads []*ast.AssignStmt
	bound := map[string][]token.Pos{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					bound[id.Name] = append(bound[id.Name], n.Pos())
				}
			}
			if len(n.Lhs) == 3 && len(n.Rhs) == 1 && calls(n.Rhs[0], "fsutil", "OpenAsFile") {
				if id, ok := n.Lhs[1].(*ast.Ident); ok && id.Name != "_" {
					identity = id.Name
				}
			}
			if len(n.Lhs) == 1 && len(n.Rhs) == 1 && selects(n.Rhs[0], "fsutil", "EntryIdentity") {
				if id, ok := n.Lhs[0].(*ast.Ident); ok {
					reader = id.Name
				}
			}
			if len(n.Rhs) == 1 {
				if call, ok := n.Rhs[0].(*ast.CallExpr); ok {
					if _, ok := call.Fun.(*ast.Ident); ok {
						reads = append(reads, n)
					}
				}
			}
		case *ast.ValueSpec:
			for _, id := range n.Names {
				bound[id.Name] = append(bound[id.Name], n.Pos())
			}
		case *ast.CallExpr:
			switch {
			case calls(n, "fsutil", "OpenAsFile"):
				opens++
			case calls(n, "os", "SameFile"):
				sameFiles = append(sameFiles, n)
			case calls(n, "os", "Lstat"), calls(n, "os", "Stat"):
				bareStats = append(bareStats, n)
			case calls(n, "updater", "LockState") && lockAt == token.NoPos:
				lockAt = n.Pos()
			}
		}
		return true
	})
	if opens != 1 {
		t.Errorf("recoverCrashedInstall calls fsutil.OpenAsFile %d times, want once: the handle whose stat is the backup's identity", opens)
	}
	for _, call := range bareStats {
		t.Errorf("recoverCrashedInstall calls os.%s at %s: an observation of the backup reads its identity as it is taken, through "+
			"fsutil.EntryIdentity", call.Fun.(*ast.SelectorExpr).Sel.Name, fset.Position(call.Pos()))
	}
	if reader == "" {
		t.Fatal("recoverCrashedInstall binds no name to fsutil.EntryIdentity: its observations of the backup do not read through it")
	}
	observed := map[string]bool{}
	for _, a := range reads {
		if a.Rhs[0].(*ast.CallExpr).Fun.(*ast.Ident).Name != reader {
			continue
		}
		if id, ok := a.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
			observed[id.Name] = true
		}
	}
	if identity == "" {
		t.Fatal("recoverCrashedInstall binds no fsutil.OpenAsFile handle's stat to a name: the backup's identity is not a handle's")
	}
	if at := bound[identity]; len(at) != 1 {
		var where []string
		for _, p := range at {
			where = append(where, fset.Position(p).String())
		}
		t.Errorf("%s is bound %d times in recoverCrashedInstall (%s), want once, by fsutil.OpenAsFile: bound again, it may be a bare Lstat's",
			identity, len(at), strings.Join(where, ", "))
	}
	if lockAt == token.NoPos {
		t.Fatal("recoverCrashedInstall takes no updater.LockState: the test shows nothing")
	}
	afterLock := 0
	for _, call := range sameFiles {
		var names []string
		for _, arg := range call.Args {
			if id, ok := arg.(*ast.Ident); ok {
				names = append(names, id.Name)
			}
		}
		switch {
		case len(names) != 2 || !slices.Contains(names, identity):
			t.Errorf("the os.SameFile at %s does not compare %s, the handle's stat: on Windows a bare Lstat's file ID is read by path "+
				"as it is compared", fset.Position(call.Pos()), identity)
		case !observed[names[0]] && !observed[names[1]]:
			t.Errorf("the os.SameFile at %s compares %s with no observation %s read: its other side's identity is not read as it "+
				"is taken", fset.Position(call.Pos()), identity, reader)
		}
		if call.Pos() > lockAt {
			afterLock++
		}
	}
	if afterLock == 0 {
		t.Errorf("none of recoverCrashedInstall's %d os.SameFile calls comes after its updater.LockState: the backup is not held to "+
			"its identity under the lock", len(sameFiles))
	}
}
