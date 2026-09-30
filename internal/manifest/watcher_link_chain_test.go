package manifest

// A configured root that is a link to a link is watched as deep as a root
// that is one link.
//
// fsnotify's kqueue backend (macOS, the BSDs) follows ONE level of a link it
// is asked to watch: it Readlinks the path and Lstats what that names. For a
// chain, what the first level names is a link again, so the root's watch was
// registered as a watch on a FILE: no Create event for anything added to the
// root, only a Write named after the root itself, which the watcher took for
// a change to a file in the root's PARENT. Files dropped into the root waited
// for the periodic scan, and each change logged `ERROR subtree scan … is not
// under any configured library root`. inotify and ReadDirectoryChangesW
// follow every level, so this runs, and passes, on every platform; it was
// red on macOS.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// runWatcherOn starts a watcher over sc (startWatcher: it returns once the
// initial walk is done, and joins the watcher at cleanup, after the test and
// before the store it writes to is closed). It slept 150 ms for the walk
// until 2026-09-29, and on a starved host the walk ended after the drop that
// followed, so no event came (backlog B104).
func runWatcherOn(t *testing.T, sc *Scanner) *Watcher {
	t.Helper()
	w, err := NewWatcher(sc, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	startWatcher(t, w)
	return w
}

// waitForPath waits for the store to hold rel, or fails with msg when the
// wait gives up or stop names a failure (watchWaitUntil).
func waitForPath(t *testing.T, store *Store, rel string, stop func() string, msg string) {
	t.Helper()
	watchWaitUntil(t, func() bool {
		st, err := store.GetTrackStat(context.Background(), rel)
		return err == nil && st != nil
	}, stop, msg)
}

// TestWatcherWatchesARootThatIsALinkToALink: a file dropped into the root
// itself, and a file dropped into a folder created in the root after the
// watcher started, both reach the manifest through the watcher, and no
// subtree scan is sent anywhere but under the configured root. Measured on
// macOS before the change: the first drop never arrived within the deadline,
// and every change to the root logged the error line about its parent.
func TestWatcherWatchesARootThatIsALinkToALink(t *testing.T) {
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, "Artist", "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	link := filepath.Join(base, "nas")
	linkDirOrSkip(t, target, link)
	chain := filepath.Join(base, "music")
	linkDirOrSkip(t, link, chain)
	store, sc := newScanFixture(t, chain)
	rec := loggingtest.Record(t)
	w := runWatcherOn(t, sc)
	// What the defect did with each drop: it asked for a scan of the root's
	// parent, which the scanner refuses. The waits below end on that line
	// rather than at the test's deadline.
	strayScan := func() string {
		for _, line := range rec.Lines("subtree scan") {
			if strings.Contains(line, "not under any configured library root") {
				return "the watcher sent a subtree scan outside the configured root: " + line
			}
		}
		return ""
	}

	writeMinimalFLAC(t, filepath.Join(chain, "dropped.flac"), 44100, 16, map[string]string{"TITLE": "Dropped"})
	waitForPath(t, store, "dropped.flac", strayScan, "a file dropped into a root that is a link to a link never reached the manifest through the watcher")

	// A folder made in the root after the watcher started is watched from
	// its Create event, which a root watched as a file never had. The drop
	// into it waits for that watch: it waited 300 ms until 2026-09-29, and
	// a drop that beats the watch reaches the manifest only through the
	// root's own scan, if at all.
	fresh := filepath.Join(chain, "New Artist")
	if err := os.Mkdir(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	watchWaitUntil(t, func() bool { return watched(w, "New Artist") }, strayScan,
		"the folder made in the root was never watched")
	writeMinimalFLAC(t, filepath.Join(fresh, "fresh.flac"), 44100, 16, map[string]string{"TITLE": "Fresh"})
	waitForPath(t, store, "New Artist/fresh.flac", strayScan, "a file dropped into a folder made in the root never reached the manifest through the watcher")

	if why := strayScan(); why != "" {
		t.Error(why)
	}
}

// TestConfiguredNameRenamesOnlyWhatIsUnderAResolvedRoot pins the renaming
// itself: a path at or below a linked root's resolved directory is named
// under the configured root, the longest resolved directory wins when two
// nest, and a sibling that merely shares the resolved directory's string
// prefix is not under it.
func TestConfiguredNameRenamesOnlyWhatIsUnderAResolvedRoot(t *testing.T) {
	p := filepath.FromSlash
	wt := &Watcher{aliases: []rootAlias{
		{resolved: p("/mnt/nas/music"), configured: p("/srv/music")},
		{resolved: p("/mnt/nas/music/live"), configured: p("/srv/live")},
	}}
	for _, c := range []struct{ in, want string }{
		{p("/mnt/nas/music"), p("/srv/music")},
		{p("/mnt/nas/music/Artist/01.flac"), p("/srv/music/Artist/01.flac")},
		{p("/mnt/nas/music/live/Set/01.flac"), p("/srv/live/Set/01.flac")},
		{p("/mnt/nas/music2/Artist/01.flac"), p("/mnt/nas/music2/Artist/01.flac")},
		{p("/srv/music/Artist/01.flac"), p("/srv/music/Artist/01.flac")},
	} {
		if got := wt.configuredName(c.in); got != c.want {
			t.Errorf("configuredName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
