package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
// answers as a backup of previousVersion that loads the settings.
type backupRuns struct {
	dataDir string
	version func(ctx context.Context) (string, error)
	check   func(ctx context.Context) updater.ConfigCheck
	asked   []string
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
	return serveOpts{configPath: r.cfg, binaryPath: r.bin, restoreBackup: restore,
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
// or whose modification time alone moved: the start says so and records it,
// and the next start of the loop asks again.
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
	for _, tc := range []struct {
		name   string
		inThe  string
		change func(t *testing.T, r *refusedInstall, replacement string)
	}{
		{"replaced during its settings check", "check", replace},
		{"replaced between its version and its check", "version", replace},
		{"rewritten to another size, its time kept", "check", rewrite},
		{"its modification time alone moved", "check", touch},
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
			requireBackupThere(t, r)
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
// nothing said, recorded or renamed over it. The same install, its marker now
// naming another version as the one it replaced, is refused in the words the
// backup's version has always been refused in, its answer held to the marker
// read again. And an install made again meanwhile, its versions and its count
// as they were and its AttemptedAt alone moved, is refused as another install
// than the one the backup was checked for.
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
