package backup_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// behindSnapshotWithConfig is a snapshot of a store one migration step behind
// this build's ladder top, with a bridge.yaml beside it, and the targets of a
// destination holding a store and a bridge.yaml of its own. A restore of it
// writes bridge.yaml after the database, in its turn of that file's lock, and
// mints the epoch through an open that migrates, which takes the install
// marker's lock (disarmBeforeMigrating).
func behindSnapshotWithConfig(t *testing.T) (snap string, dst backup.Targets) {
	t.Helper()
	fresh := filepath.Join(t.TempDir(), backup.ManifestDBFileName)
	liveStore(t, fresh)
	src := t.TempDir()
	db, cfg := filepath.Join(src, backup.ManifestDBFileName), filepath.Join(src, "bridge.yaml")
	if err := manifest.WriteDatabaseAtVersionForTest(db, userVersionOf(t, fresh)-1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("libraryName: Snapshot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: src, ManifestDB: db, BridgeYAML: cfg})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dst = backup.Targets{ManifestDB: filepath.Join(dir, backup.ManifestDBFileName), BridgeYAML: filepath.Join(dir, "bridge.yaml")}
	liveStore(t, dst.ManifestDB)
	if err := os.WriteFile(dst.BridgeYAML, []byte("libraryName: Live\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return snap, dst
}

// TestAWriterOfTheMarkerThenTheConfigMeetsNoLockCycleBesideARestore: a
// restore of a store one step behind mints its backup epoch through an open
// that migrates, which takes the install marker's lock, and mints only once it
// has let bridge.yaml's lock go. A writer that takes the marker's lock once
// the restore let it go after the database's copy, and then bridge.yaml's (the
// order every writer that takes both keeps), waits for the restore's config
// work and goes on; the epoch's open waits for the writer. Each step is a
// handshake on the locks: the writer holds the marker's lock and waits for
// bridge.yaml's before the restore goes on from its copy, and keeps both until
// the epoch's open waits for the marker's. Until B653 PR 2a's fourth round the
// epoch was minted right after the copy, bridge.yaml's lock held, and the two
// waited on each other until the writer's wait (config.LockWait) refused it.
func TestAWriterOfTheMarkerThenTheConfigMeetsNoLockCycleBesideARestore(t *testing.T) {
	snap, dst := behindSnapshotWithConfig(t)
	dataDir := filepath.Dir(dst.ManifestDB)
	t.Cleanup(updater.SetStateLockWaitForTest(20 * time.Second))
	configWaited, stop := config.LockWaitedForTest(dst.BridgeYAML)
	t.Cleanup(stop)
	markerWaited := make(chan struct{})
	var markerOnce sync.Once
	t.Cleanup(updater.SetStateLockWaitingForTest(func(d string) {
		if d == dataDir {
			markerOnce.Do(func() { close(markerWaited) })
		}
	}))

	release := make(chan struct{})
	letGo := sync.OnceFunc(func() { close(release) })
	held, writerDone, restoreDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var writerStarted atomic.Bool
	var writerErr, restoreErr error
	startWriter := sync.OnceFunc(func() {
		writerStarted.Store(true)
		go func() {
			defer close(writerDone)
			lock, err := updater.LockState(context.Background(), dataDir)
			if err != nil {
				writerErr = fmt.Errorf("take the install marker's lock: %w", err)
				return
			}
			defer lock.Release()
			close(held)
			l, err := config.LockFile(dst.BridgeYAML)
			if err != nil {
				writerErr = err
				return
			}
			defer l.Unlock()
			select {
			case <-markerWaited:
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}()
	})
	handshake := make(chan error, 1)
	opts := backup.WithAfterDatabaseCopyForTest(backup.RestoreOptions{}, func() {
		startWriter()
		for _, step := range []struct {
			name string
			ch   <-chan struct{}
		}{{"the writer holds the install marker's lock", held}, {"the writer waits for bridge.yaml's lock", configWaited}} {
			select {
			case <-step.ch:
			case <-release:
				handshake <- fmt.Errorf("let go before %s", step.name)
				return
			case <-time.After(10 * time.Second):
				handshake <- fmt.Errorf("%s: never reached", step.name)
				return
			}
		}
		handshake <- nil
	})
	go func() {
		defer close(restoreDone)
		restoreErr = backup.RestoreWithOptions(snap, dst, opts)
	}()
	t.Cleanup(func() {
		letGo()
		for _, c := range []struct {
			name string
			done <-chan struct{}
			ran  bool
		}{{"the restore", restoreDone, true}, {"the writer", writerDone, writerStarted.Load()}} {
			if !c.ran {
				continue
			}
			select {
			case <-c.done:
			case <-time.After(60 * time.Second):
				t.Errorf("%s did not end", c.name)
			}
		}
	})

	select {
	case <-restoreDone:
	case <-time.After(60 * time.Second):
		t.Fatal("the restore did not answer")
	}
	select {
	case err := <-handshake:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("the restore never reached its database's copy: %v", restoreErr)
	}
	select {
	case <-writerDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the writer did not end")
	}
	if writerErr != nil {
		t.Fatalf("a lock cycle: the restore held bridge.yaml's lock while its epoch's open waited for the install marker's, "+
			"which the writer held while it waited for bridge.yaml's, until that wait refused it: %v", writerErr)
	}
	select {
	case <-markerWaited:
	default:
		t.Error("the epoch's open never waited for the install marker's lock the writer held")
	}
	if restoreErr != nil {
		t.Fatalf("the restore = %v", restoreErr)
	}
	if epochOf(t, dst.ManifestDB) == epochOf(t, filepath.Join(snap, backup.ManifestDBFileName)) {
		t.Error("the restored database kept the snapshot's epoch")
	}
}

// TestARestoreRefusedAtBridgeYAMLStillMintsItsEpoch: a restore whose
// bridge.yaml another writer changed in its turn, the database already
// replaced, is refused at that write and still mints the restored database's
// backup epoch, once it has let bridge.yaml's lock go: a full restore writes
// no journal, so nothing else would give the restored library a new backup
// generation (B653 PR 2a's fourth round).
func TestARestoreRefusedAtBridgeYAMLStillMintsItsEpoch(t *testing.T) {
	snap, dst := behindSnapshotWithConfig(t)
	opts := backup.WithBeforeConfigForTest(backup.RestoreOptions{}, func() { edit(t, dst.BridgeYAML) })
	err := backup.RestoreWithOptions(snap, dst, opts)
	if err == nil || !strings.Contains(err.Error(), "run the restore again") {
		t.Fatalf("a restore whose bridge.yaml changed in its turn = %v, want the late refusal", err)
	}
	if epochOf(t, dst.ManifestDB) == epochOf(t, filepath.Join(snap, backup.ManifestDBFileName)) {
		t.Error("the refused restore left the restored database on the snapshot's epoch")
	}
}
