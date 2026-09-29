package manifest

import (
	"errors"
	"io/fs"
	"testing"
)

// TestDirLinkEntryTellsALinkToADirectoryByTheStatThroughIt drives
// dirLinkEntry with the shapes a walk meets, including the ones a POSIX host
// cannot make. A Windows junction or a volume mounted in a folder is
// ModeIrregular without ModeDir since Go 1.23, with no symlink bit, so a test
// for "is a symlink" would miss the ordinary Windows way to move an album to
// another volume; the stat through the entry is what says it is a directory.
// A regular file or a directory is never stat'ed again (the walk's hot path),
// and neither is a name the walk would skip as a directory.
func TestDirLinkEntryTellsALinkToADirectoryByTheStatThroughIt(t *testing.T) {
	dir := modeInfo(fs.ModeDir | 0o755)
	file := modeInfo(0o644)
	for _, tc := range []struct {
		name      string
		typ       fs.FileMode
		entry     string
		info      fs.FileInfo
		err       error
		want      bool
		wantStats int
	}{
		{"a symlink to a directory", fs.ModeSymlink, "Artist", dir, nil, true, 1},
		{"a Windows junction to a directory", fs.ModeIrregular, "Artist", dir, nil, true, 1},
		{"a link to a directory named like a track", fs.ModeSymlink, "Live.flac", dir, nil, true, 1},
		{"a symlink to a file", fs.ModeSymlink, "cover.jpg", file, nil, false, 1},
		{"a link whose target cannot be stat'ed", fs.ModeSymlink, "Artist", nil, fs.ErrNotExist, false, 1},
		{"a named pipe", fs.ModeNamedPipe, "pipe", modeInfo(fs.ModeNamedPipe), nil, false, 1},
		{"a regular file", 0, "01.flac", dir, nil, false, 0},
		{"a directory", fs.ModeDir, "Album", dir, nil, false, 0},
		{"a dot-named link to a directory", fs.ModeSymlink, ".snapshots", dir, nil, false, 0},
		{"a link named like a recycle bin", fs.ModeIrregular, "$RECYCLE.BIN", dir, nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats := 0
			through := func() (fs.FileInfo, error) {
				stats++
				return tc.info, tc.err
			}
			info, got := dirLinkEntry(tc.typ, tc.entry, through)
			if got != tc.want {
				t.Errorf("dirLinkEntry = %v, want %v", got, tc.want)
			}
			if got && (info == nil || !info.IsDir()) {
				t.Errorf("a link to a directory came back with the stat %v", info)
			}
			if stats != tc.wantStats {
				t.Errorf("%d stats through the entry, want %d", stats, tc.wantStats)
			}
		})
	}
}

// TestThroughStatStatsOnce: the walk asks dirLinkEntry and then
// walkedFileInfo about the same link, and it is stat'ed once.
func TestThroughStatStatsOnce(t *testing.T) {
	ts := throughStat{abs: "/this/path/is/not/there"}
	_, first := ts.stat()
	ts.abs = "/"
	if _, second := ts.stat(); !errors.Is(first, fs.ErrNotExist) || !errors.Is(second, fs.ErrNotExist) {
		t.Errorf("the second ask stat'ed again: %v, then %v", first, second)
	}
}

// TestDirLinkTallyHoldsOnlyWhatIsUnderALink: a row is under a link when a
// directory above it is one, by whole path segments; the link's own path is
// not held (a file that became a link is reaped like a deleted file's), nor
// is a sibling whose name starts with the link's.
func TestDirLinkTallyHoldsOnlyWhatIsUnderALink(t *testing.T) {
	var d dirLinkTally
	if _, ok := d.holding("Artist/Album/01.flac"); ok {
		t.Fatal("an empty tally holds a row")
	}
	d.note("Genre/Artist")
	d.note("music/Live.flac")
	d.note("Genre/Artist")
	if len(d.rels) != 2 {
		t.Errorf("a link noted twice is listed twice: %v", d.rels)
	}
	for p, want := range map[string]string{
		"Genre/Artist/Album/01.flac":           "Genre/Artist",
		"Genre/Artist/01.flac":                 "Genre/Artist",
		"music/Live.flac/Disc 1/Show.iso/st/1": "music/Live.flac",
		"Genre/Artist":                         "",
		"Genre/Artist 2/Album/01.flac":         "",
		"Genre/Artists/Album/01.flac":          "",
		"Genre/01.flac":                        "",
		"Artist/Album/01.flac":                 "",
	} {
		got, ok := d.holding(p)
		if got != want || ok != (want != "") {
			t.Errorf("holding(%q) = %q, %v; want %q", p, got, ok, want)
		}
	}
}
