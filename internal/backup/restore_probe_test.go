package backup_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// stepProbeRelease is the step a probe's mark names while its release does
// not end (hold.go's stepProbeRelease).
const stepProbeRelease = "letting go of the lock it tried"

// probeFolder lays out, as resolved, a snapshot in a backups folder of its
// own beside that folder's lock file, which nobody holds: a probe of the
// lock opens the file there and takes the lock. It answers the snapshot and
// the folder.
func probeFolder(t *testing.T) (snap, root string) {
	t.Helper()
	snap = fakeSnapshot(t, fmt.Sprintf(`{"schemaVersion":%d}`, backup.SchemaVersion))
	root = filepath.Dir(snap)
	f, err := fsutil.OpenLockFile(root, backup.LockFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return snap, root
}

// probeStall parks, once armed, the release of a probe of the lock of the
// backups folder at its root at the step given (SetOnProbeReleaseForTest:
// "unlock" or "close"), and hears that release's end ("released").
type probeStall struct {
	p        *parking
	armed    atomic.Bool
	parked   atomic.Bool
	released chan struct{}
	once     sync.Once
}

// stallProbeRelease installs a probeStall on the folder at root, not armed,
// and the waits its end owes at the test's end: registered after it, the
// test's join of its callers lets the release go first.
func stallProbeRelease(t *testing.T, root, step string) *probeStall {
	t.Helper()
	s := &probeStall{p: newParking(), released: make(chan struct{})}
	t.Cleanup(backup.SetOnProbeReleaseForTest(func(dir, at string) {
		if dir != root {
			return
		}
		switch {
		case at == step && s.armed.CompareAndSwap(true, false):
			s.parked.Store(true)
			s.p.park()
		case at == "released" && s.parked.Load():
			s.once.Do(func() { close(s.released) })
		}
	}))
	t.Cleanup(func() {
		s.p.release()
		if s.parked.Load() {
			awaitClosed(t, s.released, "the end of the probe's release")
		}
	})
	return s
}

// requireMarkedAs waits, within the bound, for this process's mark on the
// folder at root (StuckCheckHere, which asks no filesystem) to be one want
// takes, and fails the test with what stands there otherwise.
func requireMarkedAs(t *testing.T, root string, within time.Duration, what string, want func(backup.StuckCheck) bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		check, held := backup.StuckCheckHere(root)
		if held && want(check) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("while the probe's release does not answer the folder reads %+v, held %v; want it marked as %s", check, held, what)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// requireNoMarkerWritten fails the test where the marker beside the lock at
// path is not what it was (stale, nil for none): a probe's release past its
// bound marks its own process alone. A read of the file can come before a
// marker's write, made on a goroutine of its own (markStuck), so
// requireMarksWriteNoMarker asks the mark itself.
func requireNoMarkerWritten(t *testing.T, path string, stale []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	switch {
	case stale == nil && !errors.Is(err, os.ErrNotExist):
		t.Errorf("a marker was written beside the lock: %q, %v", got, err)
	case stale != nil && (err != nil || !bytes.Equal(got, stale)):
		t.Errorf("the marker beside the lock reads %q, %v; want it as it was, %q", got, err, stale)
	}
}

// requireMarksWriteNoMarker fails the test where no mark stands on the folder
// at root, or where one would write a marker beside its lock: asked of the
// marks themselves (MarkersForTest), once what raised them has answered.
func requireMarksWriteNoMarker(t *testing.T, root string) {
	t.Helper()
	markers := backup.MarkersForTest(root)
	if len(markers) == 0 {
		t.Error("no mark stands on the folder while the probe's release does not answer")
	}
	for _, m := range markers {
		if m != "" {
			t.Errorf("the mark of the probe's release writes a marker beside the lock, at %s", m)
		}
	}
}

// TestAProbesReleasePastItsBoundIsMarkedInItsCallersName is backlog B871's
// item 3: a probe of a backups folder's lock (StuckCheckOf's, beside a marker
// whose lock nobody holds, and ProbeBackupsLock's) whose release does not end,
// its unlock or its lock file's close stalled, answers within its bound
// (releaseBound), and marks the folder in this process alone, naming its
// release, in the name of the caller that asked: bridge restore's look at its
// snapshot (CheckRestoreSource), as that restore, whose refusal before the
// prompt names the stalled release; the doctor's and the scheduled snapshot's
// as their own probes, never a restore's, ProbeBackupsLock answering
// ErrCheckStillRunning for its own (round 2). This process's holder of the
// folder is kept until the release ends, no marker is written beside the
// lock (once the unlock has answered another process may hold it; asked of
// the mark itself as well as of the file, whose write would be on a goroutine
// of its own), and once the release ends the mark, the holder and the lock
// are all let go. Before the fix the release blocked its probe until it
// ended, unmarked, and bridge restore's refusal was the folder's that did not
// answer.
func TestAProbesReleasePastItsBoundIsMarkedInItsCallersName(t *testing.T) {
	type answer struct {
		check backup.StuckCheck
		held  bool
		err   error
	}
	const (
		restoreLook = "bridge restore's look"
		doctorHold  = "the doctor's look for a hold"
		doctorLock  = "the doctor's look for a lock"
		tickerHold  = "the scheduled snapshot's look for a hold"
	)
	for _, step := range []string{"unlock", "close"} {
		for _, by := range []string{restoreLook, doctorHold, doctorLock, tickerHold} {
			t.Run(by+", its "+step+" stalled", func(t *testing.T) {
				const bound = 300 * time.Millisecond
				t.Cleanup(backup.SetReleaseBoundForTest(bound))
				snap, root := probeFolder(t)
				markerPath := filepath.Join(root, backup.StuckMarkerName)
				var stale []byte
				if by != doctorLock {
					// StuckCheckOf probes the lock only beside a marker.
					stale = readBytes(t, writeStuckMarker(t, root, "a check whose process ended"))
				}
				attribution := map[string]string{
					restoreLook: "the restore of " + filepath.Base(snap),
					doctorHold:  "the doctor's probe of the snapshots' lock",
					doctorLock:  "the doctor's probe of the snapshots' lock",
					tickerHold:  "the scheduled snapshot's probe of the snapshots' lock",
				}[by]
				want := func(c backup.StuckCheck) bool {
					if c.Step != stepProbeRelease {
						return false
					}
					if by == restoreLook {
						return c.Restoring() && !c.Probing() && c.Snapshot == filepath.Base(snap)
					}
					return c.Probing() && !c.Restoring() && c.Probe == attribution && c.What() == attribution
				}
				s := stallProbeRelease(t, root, step)
				callers := joinBackupCallers(t, s.p.release)

				s.armed.Store(true)
				done := make(chan answer, 1)
				asked := time.Now()
				callers.start(func() {
					var a answer
					switch by {
					case restoreLook:
						a.err = backup.CheckRestoreSource(context.Background(), snap, 20*time.Second)
					case doctorHold:
						a.check, a.held = backup.StuckCheckOf(root, backup.ProbeByDoctor)
					case doctorLock:
						a.err = backup.ProbeBackupsLock(root, backup.ProbeByDoctor)
					case tickerHold:
						a.check, a.held = backup.StuckCheckOf(root, backup.ProbeByTicker)
					}
					done <- a
				})
				awaitEntered(t, s.p.entered, "the probe's "+step)
				requireMarkedAs(t, root, bound+2*time.Second, attribution, want)
				if !backup.FolderTokenHeldForTest(root) {
					t.Error("this process's holder of the folder was let go while the probe's release does not answer")
				}
				requireNoMarkerWritten(t, markerPath, stale)

				a := requireReturnedWithin(t, done, asked, bound+2*time.Second, "a probe whose release does not answer")
				requireMarksWriteNoMarker(t, root)
				switch by {
				case restoreLook:
					var held *backup.StillRunningError
					if !errors.Is(a.err, backup.ErrCheckStillRunning) || !errors.As(a.err, &held) || !want(held.Check) || backup.HeldByProbe(a.err) {
						t.Errorf("bridge restore's look answered %v, want the refusal of its own restore's release", a.err)
					}
					if a.err == nil || !strings.Contains(a.err.Error(), stepProbeRelease) || !says(a.err.Error(), snap) || !strings.Contains(a.err.Error(), "nothing was restored") {
						t.Errorf("bridge restore's look says %v, want the stalled release named, and the snapshot as given", a.err)
					}
				case doctorLock:
					// Its own release holds the folder, and it says so: no
					// caller takes a lock granted for a folder free (backlog
					// B871's round 2).
					var held *backup.StillRunningError
					if !errors.Is(a.err, backup.ErrCheckStillRunning) || !errors.As(a.err, &held) || !want(held.Check) || errors.Is(a.err, backup.ErrBackupsUnlocked) {
						t.Errorf("the doctor's probe of a lock granted, its release not ending, answered %v; want ErrCheckStillRunning naming its own release", a.err)
					}
				default:
					if !a.held || !want(a.check) {
						t.Errorf("%s answered %+v, held %v; want its own probe's release, never a restore's", by, a.check, a.held)
					}
				}

				s.p.release()
				awaitClosed(t, s.released, "the end of the probe's release")
				if c, held := backup.StuckCheckHere(root); held {
					t.Errorf("the folder is still marked %+v once the probe's release ended", c)
				}
				if backup.FolderTokenHeldForTest(root) {
					t.Error("this process's holder of the folder is still taken once the probe's release ended")
				}
				if held, err := backupsLockHeld(root); err != nil || held {
					t.Errorf("the folder's lock once the probe's release ended: held %v, %v; want it free", held, err)
				}
				requireNoMarkerWritten(t, markerPath, stale)
			})
		}
	}
}
