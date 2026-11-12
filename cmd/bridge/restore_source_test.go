package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// installState is what a restore into the install at cfgPath, whose data dir
// is dataDir, can change: what the config's folder and the data dir hold, but
// the data dir's lock, which bridge restore takes before anything else, and
// the bytes of every regular file in the data dir and of the config.
type installState struct {
	listing map[string][]string
	bytes   map[string][]byte
}

// readInstallState reads installState for the install at cfgPath.
func readInstallState(t *testing.T, cfgPath, dataDir string) installState {
	t.Helper()
	s := installState{listing: map[string][]string{}, bytes: map[string][]byte{}}
	for _, dir := range []string{filepath.Dir(cfgPath), dataDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			if e.Name() == serveLockFileName {
				continue
			}
			names = append(names, e.Name())
			if dir == dataDir && e.Type().IsRegular() {
				data, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					t.Fatal(err)
				}
				s.bytes[filepath.Join(dir, e.Name())] = data
			}
		}
		sort.Strings(names)
		s.listing[dir] = names
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	s.bytes[cfgPath] = data
	return s
}

// requireInstallUntouched fails the test for each change a refused restore
// made to the install at cfgPath since before was read.
func requireInstallUntouched(t *testing.T, cfgPath, dataDir string, before installState) {
	t.Helper()
	now := readInstallState(t, cfgPath, dataDir)
	for dir, names := range before.listing {
		if !slices.Equal(now.listing[dir], names) {
			t.Errorf("the refused restore changed what %s holds: %v, was %v", dir, now.listing[dir], names)
		}
	}
	for p, data := range before.bytes {
		if !bytes.Equal(now.bytes[p], data) {
			t.Errorf("the refused restore wrote %s", p)
		}
	}
}

// holdBackupsLockElsewhere holds the lock of the backups folder at root as
// another process's verification or prune holds it: an open of its own, which
// every other open of the file is refused beside.
func holdBackupsLockElsewhere(t *testing.T, root string) (release func()) {
	t.Helper()
	f, err := fsutil.OpenLockFile(root, backup.LockFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	release = sync.OnceFunc(func() { _ = fsutil.Unlock(f); _ = f.Close() })
	t.Cleanup(release)
	return release
}

// TestBridgeRestoreSaysWhyTheSnapshotsFolderRefusedIt: bridge restore of
// install A's snapshot into install B is refused, exit 1, B as it was, while
// the folder that holds the snapshot cannot be locked, each kind in the words
// bridge backup --verify gives it (backlog B826): a check that did not end in
// another process (its marker beside the lock it holds), another verification
// or prune holding the lock for all of the restore's wait, and a folder where
// no lock can be had (read-only media).
func TestBridgeRestoreSaysWhyTheSnapshotsFolderRefusedIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, root string)
		want []string
	}{
		{"a check that did not end in another process", func(t *testing.T, root string) {
			holdBackupsLockElsewhere(t, root)
			data, err := json.Marshal(backup.StuckCheck{Snapshot: "a snapshot", Step: "its check", Since: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, backup.StuckMarkerName), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}, []string{"did not end still holds them", "nothing was restored", "restart the bridge that keeps those snapshots"}},
		{"another verification or prune past the wait", func(t *testing.T, root string) {
			prev := verifyCmdLockWait
			verifyCmdLockWait = 200 * time.Millisecond
			t.Cleanup(func() { verifyCmdLockWait = prev })
			holdBackupsLockElsewhere(t, root)
		}, []string{"holds their lock", "nothing was restored", "run this again once it ends"}},
		{"no lock to be had", func(t *testing.T, root string) {
			t.Cleanup(backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
				if dir == root {
					return nil, &fs.PathError{Op: "open", Path: filepath.Join(dir, name), Err: syscall.EROFS}
				}
				return fsutil.OpenLockFile(dir, name)
			}))
		}, []string{"no lock could be had", "nothing was restored", "copy the snapshot"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
			cfgB, dataB, _ := seedRestorableInstall(t, closedAdminAddr(t))
			root, err := fsutil.ResolveLinks(filepath.Dir(snapA))
			if err != nil {
				t.Fatal(err)
			}
			before := readInstallState(t, cfgB, dataB)
			tc.set(t, root)
			code, stdout, stderr := runRestoreYes(t, cfgB, snapA)
			if code != 1 || strings.Contains(stdout, "Restore complete") {
				t.Errorf("bridge restore beside %s = %d\nstdout:\n%s\nstderr:\n%s", tc.name, code, stdout, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("bridge restore beside %s does not say %q:\n%s", tc.name, want, stderr)
				}
			}
			requireInstallUntouched(t, cfgB, dataB, before)
		})
	}
}

// TestBridgeRestoreRefusesBeforeItsPromptWhileAHoldGivenUpHoldsTheSnapshot: a
// hold given up in this process that still holds the folder of the snapshot
// (a prune whose removals stopped answering) refuses bridge restore at once,
// before anything reads the snapshot and before its prompt, exit 1, B as it
// was: the read could hang on the mount the hold's step stalled on. Its
// control drops that first check: the command then reads the snapshot and
// asks for its confirmation.
func TestBridgeRestoreRefusesBeforeItsPromptWhileAHoldGivenUpHoldsTheSnapshot(t *testing.T) {
	_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
	cfgB, dataB, _ := seedRestorableInstall(t, closedAdminAddr(t))
	root := filepath.Dir(snapA)
	before := readInstallState(t, cfgB, dataB)
	t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
	stalled := make(chan struct{})
	var letGo sync.Once
	t.Cleanup(backup.SetOnPruneForTest(func() { <-stalled }))
	callerDone := make(chan struct{})
	drainLoopOnCleanup(t, func() { letGo.Do(func() { close(stalled) }) }, callerDone, "backup prune")
	t.Cleanup(func() {
		letGo.Do(func() { close(stalled) })
		waitClosed(t, callerDone, "backup prune")
		awaitHoldsLetGo(t, root)
	})
	pruned := make(chan error, 1)
	go func() {
		defer close(callerDone)
		_, err := backup.Prune(root, 1)
		pruned <- err
	}()
	select {
	case err := <-pruned:
		if !errors.Is(err, backup.ErrPruneGivenUp) {
			t.Fatalf("premise: the prune answered %v, want it given up", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("premise: the prune whose removals hang was never given up")
	}
	awaitMarked(t, root)

	var stdout, stderr strings.Builder
	code := restoreCmd(context.Background(), []string{"--config", cfgB, snapA}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "did not end still holds them") || !strings.Contains(stderr.String(), "a prune") {
		t.Errorf("bridge restore beside a prune given up = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "Type 'yes'") || strings.Contains(stdout.String(), "Restore from") {
		t.Errorf("bridge restore asked for its confirmation before it refused:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	requireInstallUntouched(t, cfgB, dataB, before)
}

// TestBridgeRestoreRefusesASnapshotPrunedWhileItAsked: bridge restore reads
// the snapshot before its prompt, and the snapshot is removed while the prompt
// waits, as a prune would. Answered, the restore finds it gone under its
// folder's lock, before it completes the player seal B left incomplete, and
// is refused, exit 1, saying so: B as it was, its seal's markers included.
// Red on main, which completed the seal (player-initialized written) before it
// read the snapshot's manifest again.
func TestBridgeRestoreRefusesASnapshotPrunedWhileItAsked(t *testing.T) {
	_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
	cfgB, dataB, _ := seedRestorableInstall(t, closedAdminAddr(t))
	// A discard whose second record never landed: its next lifecycle
	// operation completes it, writing player-initialized.
	if err := os.WriteFile(filepath.Join(dataB, "player-floor-discarded.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readInstallState(t, cfgB, dataB)

	// The command and its look's worker are joined on every exit path
	// (backlog B871's item 7): their joins are registered before the prompt's
	// stdin is, whose close, a cleanup registered later, runs first and lets
	// the command go on.
	ended := lookWorkerEnd(t, snapA)
	t.Cleanup(func() { awaitRestoreEvent(t, ended, "the end of the look's worker") })
	run := newRestoreRun(t)
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	var stdout, stderr safeBuffer
	run.start(context.Background(), []string{"--config", cfgB, snapA}, pr, &stdout, &stderr)
	waitForRestorePrompt(t, &stderr, run.done)
	if err := os.RemoveAll(snapA); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write([]byte("yes\n")); err != nil {
		t.Fatal(err)
	}
	code := run.code(t)
	if code != 1 || !strings.Contains(stderr.String(), "no longer there") || !strings.Contains(stderr.String(), "choose another snapshot") {
		t.Errorf("bridge restore of a snapshot removed while it asked = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(dataB, "player-initialized")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused restore completed B's player seal: %v", err)
	}
	requireInstallUntouched(t, cfgB, dataB, before)
}
