package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// cliProbeStall parks, once armed, the release of a probe of the lock of the
// backups folder at its root at the step given (backup.SetOnProbeReleaseForTest:
// "unlock" or "close"), and hears that release's end ("released"). Its
// cleanup, registered as it is installed, lets the release go and waits for
// its end; a test's later cleanups run before it.
type cliProbeStall struct {
	entered, out, released chan struct{}
	armed, parked          atomic.Bool
	in, end                sync.Once
	letGo                  func()
}

// stallTheProbesRelease installs a cliProbeStall on the folder at root, armed.
func stallTheProbesRelease(t *testing.T, root, step string) *cliProbeStall {
	t.Helper()
	s := &cliProbeStall{entered: make(chan struct{}), out: make(chan struct{}), released: make(chan struct{})}
	s.letGo = sync.OnceFunc(func() { close(s.out) })
	s.armed.Store(true)
	t.Cleanup(backup.SetOnProbeReleaseForTest(func(dir, at string) {
		if dir != root {
			return
		}
		switch {
		case at == step && s.armed.CompareAndSwap(true, false):
			s.parked.Store(true)
			s.in.Do(func() { close(s.entered) })
			<-s.out
		case at == "released" && s.parked.Load():
			s.end.Do(func() { close(s.released) })
		}
	}))
	t.Cleanup(func() {
		s.letGo()
		if s.parked.Load() {
			awaitRestoreEvent(t, s.released, "the end of the probe's release")
		}
	})
	return s
}

// staleMarkerBesideAFreeLock makes the lock file of the backups folder at
// root, which nobody holds, and leaves beside it the marker of a check whose
// process ended, which a probe of the lock then asks about; it answers the
// marker's path and bytes.
func staleMarkerBesideAFreeLock(t *testing.T, root string) (string, []byte) {
	t.Helper()
	f, err := fsutil.OpenLockFile(root, backup.LockFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	p := writeHoldMarker(t, root, backup.StuckCheck{Snapshot: "2026-01-02T03-04-05Z", Step: "its check", Since: time.Now().UTC()})
	return p, readBytes(t, p)
}

// requireMarkerAsItWas fails the test where the marker at p is not stale:
// a probe's release marks its own process alone. A marker's write would be on
// a goroutine of its own, after the answer that read it may precede, so the
// cases read it again once the release has ended.
func requireMarkerAsItWas(t *testing.T, p string, stale []byte) {
	t.Helper()
	if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, stale) {
		t.Errorf("the marker beside the lock reads %q, %v; want it as it was, %q", got, err, stale)
	}
}

// TestBridgeRestoreNamesItsLooksStalledReleaseBeforeItsPrompt is backlog
// B871's item 3 through the command: bridge restore's look at a snapshot
// whose folder holds a marker beside a lock nobody holds takes that lock to
// ask, and its release does not end (its unlock, or separately its lock
// file's close, stalled). The command is refused before its prompt, exit 1,
// in words that name the stalled release as its own restore's, the install as
// it was and the marker beside the lock as it was: no marker is written for
// a release. "No prompt" alone proves nothing: before the fix the release
// blocked the look until its watchdog refused it, unnamed, as a folder that did
// not answer.
func TestBridgeRestoreNamesItsLooksStalledReleaseBeforeItsPrompt(t *testing.T) {
	for _, step := range []string{"unlock", "close"} {
		t.Run("its "+step+" stalled", func(t *testing.T) {
			t.Cleanup(backup.SetReleaseBoundForTest(300 * time.Millisecond))
			prev := verifyCmdLockWait
			verifyCmdLockWait = 100 * time.Millisecond
			t.Cleanup(func() { verifyCmdLockWait = prev })
			_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
			cfgB, dataB, _ := seedRestorableInstall(t, "127.0.0.1:0")
			resolved, err := fsutil.ResolveLinks(snapA)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(resolved)
			marker, stale := staleMarkerBesideAFreeLock(t, root)
			before := readInstallState(t, cfgB, dataB)
			ended := lookWorkerEnd(t, snapA)
			// Registered before the stall, so it runs once the stall's cleanup
			// has seen the release end.
			t.Cleanup(func() { requireMarkerAsItWas(t, marker, stale) })
			s := stallTheProbesRelease(t, root, step)
			run := newRestoreRun(t)
			t.Cleanup(func() {
				s.letGo()
				if run.started.Load() {
					awaitRestoreEvent(t, ended, "the end of the look's worker")
				}
			})

			var stdout, stderr safeBuffer
			run.start(context.Background(), restoreArgs(cfgB, snapA, false), strings.NewReader(""), &stdout, &stderr)
			code := run.code(t)
			msg := stderr.String()
			if code != 1 || !strings.Contains(msg, "(a restore of snapshot "+filepath.Base(resolved)) || !strings.Contains(msg, "letting go of the lock it tried") ||
				strings.Contains(msg, "did not answer") {
				t.Errorf("bridge restore beside its look's stalled release = %d, want 1 naming its own restore's release\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), msg)
			}
			if strings.Contains(msg, "Type 'yes'") || strings.Contains(stdout.String(), "Restore from") {
				t.Errorf("bridge restore asked for its confirmation beside its look's stalled release:\n%s%s", stdout.String(), msg)
			}
			requireInstallUntouched(t, cfgB, dataB, before)
			requireMarkerAsItWas(t, marker, stale)
		})
	}
}

// TestTheDoctorAndTheTickerNameTheirOwnProbesStalledRelease is backlog B871's
// item 3 through serve's own callers of the probes (doctorBackups,
// captureHeldBack): a probe of the backups folder's lock whose release does
// not end (its unlock, or separately its close) is said as the doctor's, or
// the scheduled snapshot's, own probe, the release named, and never as a
// restore's: the doctor's backup-check line, and the ticker's reason for
// taking no snapshot. Before the fix each waited on the release: the doctor
// said the folder did not answer and named no hold, and the ticker's pass
// waited until its stop.
func TestTheDoctorAndTheTickerNameTheirOwnProbesStalledRelease(t *testing.T) {
	for _, step := range []string{"unlock", "close"} {
		for _, who := range []string{"the doctor", "the scheduled snapshot"} {
			t.Run(who+", its "+step+" stalled", func(t *testing.T) {
				t.Cleanup(backup.SetReleaseBoundForTest(300 * time.Millisecond))
				dataDir := t.TempDir()
				root := filepath.Join(dataDir, backup.BackupsDirName)
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
				marker, stale := staleMarkerBesideAFreeLock(t, root)
				// Registered before the stall, so it runs once the stall's
				// cleanup has seen the release end.
				t.Cleanup(func() { requireMarkerAsItWas(t, marker, stale) })
				s := stallTheProbesRelease(t, root, step)
				words := who + "'s probe of the snapshots' lock"
				if who == "the doctor" {
					prevEnd := onDoctorBackupsReadEnd
					readEnded := make(chan struct{})
					onDoctorBackupsReadEnd = sync.OnceFunc(func() { close(readEnded) })
					t.Cleanup(func() { onDoctorBackupsReadEnd = prevEnd })
					t.Cleanup(func() {
						s.letGo()
						awaitRestoreEvent(t, readEnded, "the end of the doctor's read of the backups folder")
					})
					var line doctor.Check
					for _, c := range doctorBackups(&config.Config{DataDir: dataDir})(context.Background()) {
						if c.Name == "backup-check" {
							line = c
						}
					}
					if !strings.Contains(line.Summary, words+" did not end (letting go of the lock it tried)") || strings.Contains(line.Summary+line.Hint, "restore") {
						t.Errorf("the doctor's backup-check line = %+v; want it naming %s and its release, and no restore", line, words)
					}
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					t.Cleanup(cancel)
					why, held := captureHeldBack(ctx, root)
					if !held || !strings.Contains(why, words+" that did not end still holds the backups folder") || strings.Contains(why, "restore") {
						t.Errorf("the ticker's reason = %q, %v; want it naming %s, and no restore", why, held, words)
					}
				}
				requireMarkerAsItWas(t, marker, stale)
			})
		}
	}
}
