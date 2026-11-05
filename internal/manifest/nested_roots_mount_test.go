//go:build darwin

package manifest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// B651's ninth round (Codex on round 8): a nested root held while its volume
// is away is watched again, once the volume is mounted over its bare folder,
// on the volume, never on the folder the mount covers. On kqueue fsnotify
// opens every entry of a folder it lists, the held root's bare folder among
// them, and its Add of the root reused that descriptor by path.
//
// A real mount, never a rename: a folder renamed over the bare one drops the
// bare folder's last link, which kqueue reports on the descriptor fsnotify
// keeps on it, and fsnotify then forgets the path, so its next Add opens a new
// descriptor and nothing shows. A mount covers the folder and reports nothing
// on it.

// diskImage is a disk image of a few megabytes, made and attached with
// hdiutil, which macOS lets any user run: the volume a test mounts on a folder
// and unmounts.
type diskImage struct {
	hdiutil, path string
	// mounted is the folder the image is attached at, "" while it is not.
	mounted string
}

// newDiskImage makes an image in a folder of its own, and detaches it, if it
// is still attached, when the test ends: after the watcher's join, which a
// test registers later, and before the temporary folders are removed. A host
// whose hdiutil refuses to make one skips the test, saying so.
func newDiskImage(t *testing.T) *diskImage {
	t.Helper()
	hdiutil, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skipf("no hdiutil on this host, so no disk image to mount: %v", err)
	}
	d := &diskImage{hdiutil: hdiutil, path: filepath.Join(t.TempDir(), "b651.dmg")}
	out, err := exec.Command(hdiutil, "create", "-quiet", "-size", "8m", "-fs", "HFS+", "-volname", "B651", d.path).CombinedOutput()
	if err != nil {
		t.Skipf("hdiutil refused to make a disk image: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if d.mounted != "" {
			_ = exec.Command(d.hdiutil, "detach", "-quiet", "-force", d.mounted).Run()
		}
	})
	return d
}

// attach mounts the image on the folder at. The first attach that hdiutil
// refuses skips the test, saying so; a later one fails it.
func (d *diskImage) attach(t *testing.T, at string, first bool) {
	t.Helper()
	out, err := exec.Command(d.hdiutil, "attach", "-quiet", "-nobrowse", "-noautoopen", "-noverify", "-mountpoint", at, d.path).CombinedOutput()
	switch {
	case err != nil && first:
		t.Skipf("hdiutil refused to attach a disk image: %v: %s", err, out)
	case err != nil:
		t.Fatalf("hdiutil attach: %v: %s", err, out)
	}
	d.mounted = at
}

// detach unmounts the image, leaving the folder it covered.
func (d *diskImage) detach(t *testing.T) {
	t.Helper()
	if out, err := exec.Command(d.hdiutil, "detach", "-quiet", "-force", d.mounted).CombinedOutput(); err != nil {
		t.Fatalf("hdiutil detach: %v: %s", err, out)
	}
	d.mounted = ""
}

// fileID is a file's device and inode number.
type fileID struct{ dev, ino uint64 }

func fileIDOf(t *testing.T, p string) fileID {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return fileID{uint64(st.Dev), st.Ino}
}

// openFilesOn is how many descriptors this process holds open on the file id:
// those /dev/fd names whose device and inode are its. A descriptor closed
// between the listing and its stat is skipped.
func openFilesOn(t *testing.T, id fileID) int {
	t.Helper()
	d, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Fstat(fd, &st) != nil {
			continue
		}
		if (fileID{uint64(st.Dev), st.Ino}) == id {
			n++
		}
	}
	return n
}

// eventNames records the base name of every event the watcher handled
// (afterEventForTests, on Run's goroutine).
type eventNames struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (e *eventNames) note(ev fsnotify.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen[filepath.Base(ev.Name)] = true
}

// both reports, at one moment, whether events named a and b were handled.
func (e *eventNames) both(a, b string) (bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seen[a], e.seen[b]
}

// TestTheWatcherWatchesAHeldNestedRootOnTheVolumeMountedOverItsBareFolder: a
// root nested in another, music/usb/library, is a volume of its own, a disk
// image here; indexed while it was mounted, it is unmounted before the watcher
// starts, so the watcher holds it and the walk of the outer root passes over
// its bare folder, which kqueue's listing of music/usb opens all the same.
// Once the volume is mounted over that folder again, the watcher watches the
// root on the volume: it holds nothing open on the folder the volume covers,
// and a file made directly under the mounted root is seen, ahead of one made
// after it in a folder below. Watched on the covered folder, the root missed
// the first while its folders, opened through the path, saw the second, and
// the check of the roots' own watches found the root's path registered.
func TestTheWatcherWatchesAHeldNestedRootOnTheVolumeMountedOverItsBareFolder(t *testing.T) {
	dir := t.TempDir()
	outer := filepath.Join(dir, "music")
	inner := filepath.Join(outer, "usb", "library")
	writeTracks(t, filepath.Join(outer, "Artist", "Album", "01.flac"))
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	img := newDiskImage(t)
	img.attach(t, inner, true)
	writeTracks(t, filepath.Join(inner, "Band", "Record", "01.flac"))
	store := openStoreIn(t, dir)
	scanner := NewScanner([]string{outer, inner}, store, "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustHoldRows(t, store, "music/usb/library/Band/Record/01.flac", "library/Band/Record/01.flac")
	img.detach(t)
	bare := fileIDOf(t, inner)
	mustReadAsNotMounted(t, scanner, inner)

	w, err := NewWatcher(scanner, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if w.fds == nil {
		t.Fatal("premise: fsnotify does not watch through kqueue on this host")
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	held, resumed := make(chan string, 8), make(chan string, 8)
	w.afterHoldForTests = func(root string) { held <- root }
	w.afterResumeForTests = func(root string) { resumed <- root }
	events := &eventNames{seen: make(map[string]bool)}
	w.afterEventForTests = events.note
	startWatcher(t, w)
	if got := waitForRoot(t, held, "the watcher never held the nested root"); got != inner {
		t.Fatalf("the watcher held %q, want %q", got, inner)
	}
	if openFilesOn(t, bare) == 0 {
		t.Fatalf("premise: the walk of %s holds nothing open on %s's bare folder", outer, inner)
	}

	img.attach(t, inner, false)
	if got := waitForRoot(t, resumed, "the watcher never watched the nested root again once its volume was mounted"); got != inner {
		t.Fatalf("the watcher watched %q again, want %q", got, inner)
	}
	if n := openFilesOn(t, bare); n != 0 {
		t.Errorf("the watcher still holds %d open file(s) on the folder the volume covers at %s", n, inner)
	}
	// Made in this order: the root's own event comes ahead of the one below
	// it, so the second without the first means the first is not coming.
	const atRoot, below = "b651-at-root.flac", "b651-below.flac"
	writeTracks(t, filepath.Join(inner, atRoot))
	writeTracks(t, filepath.Join(inner, "Band", "Record", below))
	watchWaitUntil(t, func() bool {
		sawRoot, _ := events.both(atRoot, below)
		return sawRoot
	}, func() string {
		if sawRoot, sawBelow := events.both(atRoot, below); sawBelow && !sawRoot {
			return "it saw the file made after it in a folder below, so the root's own watch is not on the mounted volume"
		}
		return ""
	}, "the watcher saw no event for a file made directly under the nested root once its volume was mounted again")
}
