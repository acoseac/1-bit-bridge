package manifest

import (
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestTheWatchBudgetIsHalfTheProcessLimitAndAQuarterOfTheSystems pins the
// share a kqueue watcher's watches may take: half of what the process may
// hold open, and a quarter of the system's table at most. With the macOS
// defaults (61,440 and 122,880) the two agree; the system's cap binds only
// where kern.maxfilesperproc is raised toward kern.maxfiles.
func TestTheWatchBudgetIsHalfTheProcessLimitAndAQuarterOfTheSystems(t *testing.T) {
	for _, c := range []struct {
		name   string
		limits WatchFDLimits
		want   int
		capped bool
	}{
		{"the macOS defaults", WatchFDLimits{Process: 61440, System: 122880}, 30720, false},
		{"a process limit raised to the system's", WatchFDLimits{Process: 122880, System: 122880}, 30720, true},
		{"a low hard limit", WatchFDLimits{Process: 159, System: 122880}, 79, false},
		{"a system table that could not be read", WatchFDLimits{Process: 61440}, 30720, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.limits.Budget(); got != c.want {
				t.Errorf("Budget() = %d, want %d", got, c.want)
			}
			if got := c.limits.SystemCapped(); got != c.capped {
				t.Errorf("SystemCapped() = %v, want %v", got, c.capped)
			}
		})
	}
}

// TestClampFileLimitReadsInfinityAsAnInt32: RLIM_INFINITY, and anything an
// int32 cannot hold, is a limit no process reaches, so it becomes
// math.MaxInt32 rather than an int that overflows a budget negative.
func TestClampFileLimitReadsInfinityAsAnInt32(t *testing.T) {
	for _, c := range []struct {
		in   uint64
		want int
	}{
		{0, 0},
		{61440, 61440},
		{math.MaxInt32, math.MaxInt32},
		{math.MaxInt32 + 1, math.MaxInt32},
		{1<<63 - 1, math.MaxInt32},
		{math.MaxUint64, math.MaxInt32},
	} {
		if got := clampFileLimit(c.in); got != c.want {
			t.Errorf("clampFileLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestKqueueHoldsEntryLeavesOutSocketsAndPipes mirrors the kqueue backend:
// it opens every entry of a folder it lists but a socket and a named pipe.
func TestKqueueHoldsEntryLeavesOutSocketsAndPipes(t *testing.T) {
	for _, c := range []struct {
		mode fs.FileMode
		want bool
	}{
		{0, true},
		{fs.ModeDir, true},
		{fs.ModeSymlink, true},
		{fs.ModeDevice | fs.ModeCharDevice, true},
		{fs.ModeSocket, false},
		{fs.ModeNamedPipe, false},
	} {
		if got := kqueueHoldsEntry(c.mode); got != c.want {
			t.Errorf("kqueueHoldsEntry(%v) = %v, want %v", c.mode, got, c.want)
		}
	}
}

// TestTheWatchAccountReleasesAFoldersWholeSubtree: releasing a folder
// returns it and everything recorded below it, folders and files at every
// depth, for the watcher to remove from fsnotify, and forgets them all, so
// the count the budget is checked against falls by as many. A path the
// account never held is still returned, for fsnotify to answer with an
// error, and moves the count by nothing.
func TestTheWatchAccountReleasesAFoldersWholeSubtree(t *testing.T) {
	p := filepath.FromSlash
	f := newWatchFDs(WatchFDLimits{Process: 1000}, nil)
	root, artist, album := p("/lib"), p("/lib/Artist"), p("/lib/Artist/Album")
	for _, dir := range []string{root, artist, album} {
		f.hold(dir)
		f.list(dir)
	}
	for _, file := range []string{p("/lib/Artist/Album/01.flac"), p("/lib/Artist/Album/02.flac"), p("/lib/Artist/cover.jpg"), p("/lib/notes.txt")} {
		f.hold(file)
	}
	if f.n != 7 {
		t.Fatalf("the account holds %d paths, want 7", f.n)
	}

	got := f.release(artist)
	slices.Sort(got)
	want := []string{artist, album, p("/lib/Artist/Album/01.flac"), p("/lib/Artist/Album/02.flac"), p("/lib/Artist/cover.jpg")}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("release(%s) = %q, want %q", artist, got, want)
	}
	if f.n != 2 {
		t.Errorf("after the release the account holds %d paths, want 2 (the root and its file)", f.n)
	}
	for _, gone := range want {
		if f.held(gone) {
			t.Errorf("%s is still held after its folder was released", gone)
		}
	}
	if f.listed(album) || f.listed(artist) {
		t.Error("a released folder is still listed")
	}

	if got := f.release(p("/lib/never")); !slices.Equal(got, []string{p("/lib/never")}) || f.n != 2 {
		t.Errorf("release of a path never held = %q with %d held, want just the path and 2", got, f.n)
	}

	// A folder waiting for its watch goes with a renamed folder at or above
	// it, and not with a sibling whose name merely starts the same.
	for _, d := range []string{p("/lib/Music"), p("/lib/Music/Disc 1"), p("/lib/MusicAndMore")} {
		f.wait(d)
	}
	f.dropPending(p("/lib/Music"))
	if got := f.pendingInOrder(); !slices.Equal(got, []string{p("/lib/MusicAndMore")}) {
		t.Errorf("after dropping /lib/Music the waiting folders are %q, want only /lib/MusicAndMore", got)
	}
}

// TestCountWatchSetCountsWhatTheWatcherHolds counts a tree the way the
// watcher plans its watches: a folder it lists, and every entry in such a
// folder (a file, a dot-file, a folder it skips without entering), each one
// open file; nothing inside a skipped folder; and a count that stops at
// stopAt says it is partial.
func TestCountWatchSetCountsWhatTheWatcherHolds(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"Artist/Album", ".Trashes/501/Old", "@eaDir/01.flac"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{"Artist/Album/01.flac", "Artist/Album/02.flac", "Artist/cover.jpg", ".DS_Store", ".Trashes/501/Old/x.flac"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(file)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The root, Artist and Album are listed; 01, 02, cover.jpg, .DS_Store,
	// .Trashes and @eaDir are entries; nothing below .Trashes or @eaDir is.
	got, err := CountWatchSet([]string{root}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Folders != 3 || got.Entries != 6 || got.Partial {
		t.Errorf("CountWatchSet = %+v, want 3 folders, 6 entries, complete", got)
	}

	got, err = CountWatchSet([]string{root}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.OpenFiles() != 4 || !got.Partial {
		t.Errorf("CountWatchSet stopping at 4 = %+v, want 4 open files, partial", got)
	}

	if _, err := CountWatchSet([]string{filepath.Join(root, "missing")}, 0); err == nil {
		t.Error("CountWatchSet of a root that is not there answered no error")
	}
}
