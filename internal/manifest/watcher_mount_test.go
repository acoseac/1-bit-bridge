//go:build darwin

package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// Backlog B777 on a real mount: a disk image attached at a library root's
// folder (newDiskImage, hdiutil attach -mountpoint, which macOS lets any user
// run), watched through kqueue. On Linux a mount needs rights the test hosts
// lack, so these are darwin's; watcher_root_identity_test.go runs the check's
// logic everywhere, through a stand-in for the identity read and a linked
// root's link repointed.

// TestTheWatcherWatchesTheVolumeMountedOverARootItWatchedBare (B777 item 1): a
// root the index holds nothing of (a fresh install whose volume is not
// mounted yet) reads as it is and is watched on its bare mount point. The
// volume then mounted over it covers the folder the watch is on, and kqueue
// says nothing. The check finds the root's path another folder, watches the
// volume, with a scan, and a file written on the volume after that reaches
// the manifest.
func TestTheWatcherWatchesTheVolumeMountedOverARootItWatchedBare(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "music")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	img := newDiskImage(t)
	img.attach(t, root, true)
	writeTracks(t, filepath.Join(root, "Artist", "Album", "01.flac"))
	img.detach(t)
	store := openStoreIn(t, dir)
	w, err := NewWatcher(NewScanner([]string{root}, store, ""), 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	rec := loggingtest.Record(t)
	startWatcher(t, w)
	if !watched(w, "music") {
		t.Fatalf("premise: the bare mount point is not watched (watches: %q)", w.w.WatchList())
	}

	img.attach(t, root, false)
	waitForRow(t, store, "Artist/Album/01.flac", "the volume mounted over a root the watcher watched bare was never scanned")
	writeTracks(t, filepath.Join(root, "Artist", "Album", "02.flac"))
	waitForRow(t, store, "Artist/Album/02.flac", "a file written on the volume mounted over the root never reached the manifest")
	if got := rec.Lines(msgWatchRootReplaced); len(got) != 1 {
		t.Errorf("the watcher said %d times that it watched the root again, want once:\n%v", len(got), got)
	}
}

// TestTheWatcherWatchesTheVolumeMountedOverANestedRootItWatchedBare is the
// same for a root nested in another, which is what kqueue's entry watches
// bear on: the walk of the outer root opens the nested root's bare mount point
// as an entry of the folder above, and fsnotify's Add of the nested root finds
// that descriptor by its path and keeps it (B651's ninth round). So the nested
// root's identity is taken before any walk, and its watch is added again only
// once what is held at its path is let go (watchAgain's releaseRoot): an Add
// over a path fsnotify holds reuses what it holds. Once the volume is mounted,
// nothing is held open on the folder it covers, and a file written directly in
// the mounted root is seen.
func TestTheWatcherWatchesTheVolumeMountedOverANestedRootItWatchedBare(t *testing.T) {
	dir := t.TempDir()
	outer := filepath.Join(dir, "music")
	inner := filepath.Join(outer, "usb")
	writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"))
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	img := newDiskImage(t)
	img.attach(t, inner, true)
	writeTracks(t, filepath.Join(inner, "Band", "Record", "01.flac"))
	img.detach(t)
	bare := fileIDOf(t, inner)
	store := openStoreIn(t, dir)
	scanner := NewScanner([]string{outer, inner}, store, "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if look := scanner.lookAtRoot(context.Background(), inner); look.absent {
		t.Fatalf("premise: the nested root reads as a volume not mounted (%s), so no watcher watches its bare mount point", look.reason)
	}
	w, err := NewWatcher(scanner, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if w.fds == nil {
		t.Fatal("premise: fsnotify does not watch through kqueue on this host")
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	events := &eventNames{seen: make(map[string]bool)}
	w.afterEventForTests = events.note
	startWatcher(t, w)
	if openFilesOn(t, bare) == 0 {
		t.Fatalf("premise: nothing is held open on %s's bare folder", inner)
	}

	img.attach(t, inner, false)
	waitForRow(t, store, "usb/Band/Record/01.flac", "the volume mounted over a nested root the watcher watched bare was never scanned")
	watchWaitUntil(t, func() bool { return openFilesOn(t, bare) == 0 }, nil,
		"the watcher still holds an open file on the folder the volume covers")
	const atRoot = "b777-at-root.flac"
	writeTracks(t, filepath.Join(inner, atRoot))
	watchWaitUntil(t, func() bool {
		saw, _ := events.both(atRoot, "")
		return saw
	}, nil, "the watcher saw no event for a file made directly under the nested root once its volume was mounted")
}

// TestTheWatcherWatchesARootRemountedInsideTheDebounce (B777 item 3): a root
// watched on its volume; the volume unmounted and mounted again while Run's
// goroutine is parked in an event's handling, so no look sees it away, as
// within one debounce. kqueue keeps the watches on the first mount's revoked
// folders, all of them still listed, so only the identity of the folder at the
// root's path can tell. It is logged, whether the remount changed the root
// folder's device and inode; where it changed neither, identity cannot tell
// the two mounts apart, and the test skips, saying so: then this case is left
// (B777).
func TestTheWatcherWatchesARootRemountedInsideTheDebounce(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "music")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	img := newDiskImage(t)
	img.attach(t, root, true)
	writeTracks(t, filepath.Join(root, "Artist", "Album", "01.flac"))
	store := openStoreIn(t, dir)
	scanner := NewScanner([]string{root}, store, "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustHoldRows(t, store, "Artist/Album/01.flac")
	w, err := NewWatcher(scanner, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	park := parkRun(w)
	startWatcher(t, w)
	t.Cleanup(park.free)
	before := fileIDOf(t, root)

	park.armed.Store(true)
	writeTracks(t, filepath.Join(root, "Artist", "Album", "park.flac"))
	select {
	case <-park.parked:
	case <-watchGiveUp(t, watchWaitReserve):
		t.Fatal("no event reached the watcher's loop to park it")
	}
	img.detach(t)
	img.attach(t, root, false)
	after := fileIDOf(t, root)
	t.Logf("the root's folder was device %d inode %d on the first mount, device %d inode %d on the second", before.dev, before.ino, after.dev, after.ino)
	if after == before {
		park.free()
		t.Skip("B777 item 3 left: the remount gave the root's folder the device and inode it had, so its identity cannot tell the two mounts apart, while kqueue's watches sit on the first mount's revoked folders")
	}
	park.free()
	writeTracks(t, filepath.Join(root, "Artist", "Album", "02.flac"))
	waitForRow(t, store, "Artist/Album/02.flac", "a file written on the volume mounted again never reached the manifest")
}
