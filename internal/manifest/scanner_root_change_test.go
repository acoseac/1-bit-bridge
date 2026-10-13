package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestARootChangeDuringAScanDoesNotLeaveTheOldRows parks a scan on the
// extract hook and changes the roots before that scan can write. The
// removed root's rows, and a collapsed library's old multi-root paths,
// must stay gone after the scan that follows. The missing-count threshold
// is 3, so a second scan only counts a reinserted row missing and the
// assertion still sees it.
func TestARootChangeDuringAScanDoesNotLeaveTheOldRows(t *testing.T) {
	t.Run("collapse", func(t *testing.T) {
		runRootChangeDuringScan(t, []string{"Keep", "Drop"}, []string{"Keep", "Drop"},
			[]string{"Album/01.flac"},
			[]string{"Drop/Album/01.flac", "Keep/Album/01.flac"},
			func(ctx context.Context, store *Store, sc *Scanner, abs map[string]string) error {
				if _, err := store.RecordFirstIndexedCarryAndWipe(ctx, true, "Keep"); err != nil {
					return err
				}
				sc.SetRoots([]string{abs["Keep"]})
				return nil
			})
	})
	t.Run("remove", func(t *testing.T) {
		runRootChangeDuringScan(t, []string{"Keep", "Drop", "Other"}, []string{"Keep", "Drop", "Other"},
			[]string{"Keep/Album/01.flac", "Other/Album/01.flac"},
			[]string{"Drop/Album/01.flac"},
			func(ctx context.Context, store *Store, sc *Scanner, abs map[string]string) error {
				if _, err := store.DeleteTracksByPrefix(ctx, "Drop/"); err != nil {
					return err
				}
				sc.SetRoots([]string{abs["Keep"], abs["Other"]})
				return nil
			})
	})
	t.Run("add", func(t *testing.T) {
		runRootChangeDuringScan(t, []string{"Keep"}, []string{"Keep", "Extra"},
			[]string{"Keep/Album/01.flac", "Extra/Album/01.flac"},
			[]string{"Album/01.flac"},
			func(ctx context.Context, store *Store, sc *Scanner, abs map[string]string) error {
				if _, err := store.RecordFirstIndexedCarryAndWipe(ctx, false, "Keep"); err != nil {
					return err
				}
				sc.SetRoots([]string{abs["Keep"], abs["Extra"]})
				return nil
			})
	})
}

func runRootChangeDuringScan(t *testing.T, initial, withFiles, present, absent []string, mutate func(context.Context, *Store, *Scanner, map[string]string) error) {
	t.Helper()
	base := t.TempDir()
	abs := map[string]string{}
	var roots []string
	for _, name := range withFiles {
		dir := filepath.Join(base, name)
		abs[name] = dir
		album := filepath.Join(dir, "Album")
		if err := os.MkdirAll(album, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(album, "01.flac"), []byte("not-a-real-flac"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range initial {
		roots = append(roots, abs[name])
	}

	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sc := NewScanner(roots, store, "")
	sc.SetDeleteThreshold(3)

	removalDone := make(chan struct{})
	var closeRemoval sync.Once
	release := func() { closeRemoval.Do(func() { close(removalDone) }) }
	defer release()

	parked, scanDone := sc.StartParkedScanForTests(removalDone)
	t.Cleanup(func() { sc.SetAfterExtractHookForTests(nil) })
	defer func() {
		select {
		case <-scanDone:
		case <-time.After(5 * time.Second):
		}
	}()
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("scan did not reach the extract hook")
	}

	sc.CommitRootChange(func() {
		if err := mutate(context.Background(), store, sc, abs); err != nil {
			t.Errorf("root change: %v", err)
		}
	})
	release()
	var scanErr error
	select {
	case scanErr = <-scanDone:
	case <-time.After(30 * time.Second):
		t.Fatal("parked scan did not finish")
	}
	if scanErr != nil && !errors.Is(scanErr, context.Canceled) {
		t.Fatal(scanErr)
	}
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	assertRootChangePaths(t, store, present, absent)
}

// TestAWaitingScanDoesNotHideTheScanARootChangeCancels parks scan A on the
// extract hook, then starts scan B and holds it at the scanner lock. A root
// change must cancel A. A scan that has not taken the lock must not be the
// one the change cancels, or the change waits out A.
func TestAWaitingScanDoesNotHideTheScanARootChangeCancels(t *testing.T) {
	store, sc, abs := openThreeRootLibrary(t)
	defer store.Close()

	releaseCh := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCh) }) }
	parked, scanDone := sc.StartParkedScanForTests(releaseCh)
	t.Cleanup(func() { sc.SetAfterExtractHookForTests(nil) })
	wait := &rootChangeJoin{release: release, scanDone: scanDone}
	defer wait.finish(t)

	waitForSignal(t, parked, "scan did not reach the extract hook")

	atLock := make(chan struct{})
	var once sync.Once
	sc.SetBeforeScanLockForTests(func() { once.Do(func() { close(atLock) }) })
	t.Cleanup(func() { sc.SetBeforeScanLockForTests(nil) })
	wait.bDone = startBlockedScan(sc)
	waitForSignal(t, atLock, "second scan did not reach the lock")

	wait.changed = dropRootWhileScanning(t, store, sc, abs)
	waitForSignal(t, wait.changed, "root change did not return while a scan waited on the lock")

	release()
	waitScanOutcome(t, scanDone, "parked scan did not finish")
	waitScanOutcome(t, wait.bDone, "waiting scan did not finish")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertRootChangePaths(t, store,
		[]string{"Keep/Album/01.flac", "Other/Album/01.flac"},
		[]string{"Drop/Album/01.flac"})
}

type rootChangeJoin struct {
	release  func()
	scanDone <-chan error
	bDone    <-chan error
	changed  <-chan struct{}
}

func (w *rootChangeJoin) finish(t *testing.T) {
	t.Helper()
	w.release()
	waitScanOutcome(t, w.scanDone, "parked scan did not finish")
	if w.bDone != nil {
		waitScanOutcome(t, w.bDone, "waiting scan did not finish")
	}
	if w.changed != nil {
		waitForSignal(t, w.changed, "root change did not return while a scan waited on the lock")
	}
}

func openThreeRootLibrary(t *testing.T) (*Store, *Scanner, map[string]string) {
	t.Helper()
	base := t.TempDir()
	abs := map[string]string{}
	var roots []string
	for _, name := range []string{"Keep", "Drop", "Other"} {
		dir := filepath.Join(base, name)
		abs[name] = dir
		album := filepath.Join(dir, "Album")
		if err := os.MkdirAll(album, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(album, "01.flac"), []byte("not-a-real-flac"), 0o644); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, dir)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	sc := NewScanner(roots, store, "")
	sc.SetDeleteThreshold(3)
	return store, sc, abs
}

func startBlockedScan(sc *Scanner) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := sc.Scan(context.Background())
		done <- err
		close(done)
	}()
	return done
}

func dropRootWhileScanning(t *testing.T, store *Store, sc *Scanner, abs map[string]string) <-chan struct{} {
	t.Helper()
	changed := make(chan struct{})
	go func() {
		sc.CommitRootChange(func() {
			if _, err := store.DeleteTracksByPrefix(context.Background(), "Drop/"); err != nil {
				t.Errorf("root change: %v", err)
			}
			sc.SetRoots([]string{abs["Keep"], abs["Other"]})
		})
		close(changed)
	}()
	return changed
}

func waitForSignal(t *testing.T, ch <-chan struct{}, timeoutMsg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatal(timeoutMsg)
	}
}

func waitScanOutcome(t *testing.T, done <-chan error, timeoutMsg string) {
	t.Helper()
	select {
	case err, ok := <-done:
		if !ok {
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal(timeoutMsg)
	}
}

func assertRootChangePaths(t *testing.T, store *Store, present, absent []string) {
	t.Helper()
	got, err := store.TrackPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	for _, p := range present {
		if !have[p] {
			t.Errorf("missing %s in %v", p, got)
		}
	}
	for _, p := range absent {
		if have[p] {
			t.Errorf("still present %s in %v", p, got)
		}
	}
}
