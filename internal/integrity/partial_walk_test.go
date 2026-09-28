package integrity

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// What a forward sweep may decide from a walk that could not read part of
// its tree (2026-09-28). A directory the walk could not list is an
// UNBOUNDED unknown and refuses (PartialWalkRefusal); a link it could not
// stat is at most one file and is weighed (MassOrphanRefusalFor); and the
// root-owned lost+found of an ext4 volume mounted as the variants directory
// is the filesystem's, not an unknown at all (isFilesystemLostFound).

// lockDir makes dir unlistable by this user until the test ends.
func lockDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// linkThroughALockedDir plants, at link, a symlink to a directory behind a
// directory this user cannot search, so a stat of the link fails with a
// permission error: the entry the inventory counts as Unreadable but not
// as an unlisted directory. Skips where the fixture cannot build that.
func linkThroughALockedDir(t *testing.T, link string) {
	t.Helper()
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(blocked, "sub"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	lockDir(t, blocked)
	if _, err := os.Stat(link); err == nil {
		t.Skip("this user can stat through a 0000 directory — the fixture cannot reproduce the state")
	}
}

// TestTakeSidecarInventoryLeavesTheFilesystemsLostFoundOut — mke2fs puts a
// root-owned, 0700 lost+found at the top of every ext2/3/4 filesystem, so a
// variants directory that IS such a volume's mount point holds one its
// service user can never list. Counted as an unlisted directory it made the
// background sweep refuse every tick, forever (#1063), and every `--gc` a
// partial walk. It is the filesystem's: not counted, at the top of the
// walk root, through a symlinked root too (the rule reads the RESOLVED
// root, where the volume actually is). A lost+found further down (a volume
// mounted inside the tree) still counts, and a readable one at the top is
// walked as it always was.
func TestTakeSidecarInventoryLeavesTheFilesystemsLostFoundOut(t *testing.T) {
	skipWhereModesDenyNothing(t)
	base := t.TempDir()
	root := filepath.Join(base, "variants")
	seedTree(t, root,
		"Artist/Album/01.flac.upscaled-v2-176400-24.flac",
		"lost+found/#12345",
		"Artist/lost+found/#678",
	)
	top, nested := filepath.Join(root, "lost+found"), filepath.Join(root, "Artist", "lost+found")
	lockDir(t, top)
	lockDir(t, nested)
	link := filepath.Join(base, "variants-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	for _, r := range []string{root, link} {
		inv, err := TakeSidecarInventory(context.Background(), r, nil, SidecarInventoryOptions{})
		if err != nil {
			t.Fatalf("walk of %s: %v", r, err)
		}
		if inv.Unreadable != 1 || inv.UnlistedDirs != 1 {
			t.Errorf("walk of %s: unreadable=%d unlistedDirs=%d, want 1 and 1 — the nested lost+found, and not the "+
				"filesystem's own at the top", r, inv.Unreadable, inv.UnlistedDirs)
		}
		if inv.Files != 1 {
			t.Errorf("walk of %s: files=%d, want the one readable sidecar", r, inv.Files)
		}
	}

	// A lost+found this user CAN list is walked as before: the rule is
	// about what the walk cannot see, not a prune.
	if err := os.Chmod(top, 0o755); err != nil {
		t.Fatal(err)
	}
	inv, err := TakeSidecarInventory(context.Background(), root, nil, SidecarInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Files != 2 || inv.UnlistedDirs != 1 {
		t.Errorf("with the top lost+found readable: files=%d unlistedDirs=%d, want 2 (its file walked) and 1 (the nested one)",
			inv.Files, inv.UnlistedDirs)
	}
}

// fakeDirEntry is the fs.DirEntry isFilesystemLostFound reads: a name.
type fakeDirEntry struct {
	fs.DirEntry
	name string
}

func (f fakeDirEntry) Name() string { return f.name }

// TestIsFilesystemLostFoundReadsAllThreeTerms pins each term of the rule,
// the error's kind included, which a real walk cannot vary: fsck's
// directory keeps a non-root user out with EACCES, and nothing else
// earns the pass (an I/O error on it is a fault like any other).
func TestIsFilesystemLostFoundReadsAllThreeTerms(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "variants")
	denied := &fs.PathError{Op: "open", Path: "x", Err: syscall.EACCES}
	for _, c := range []struct {
		name, path, entry string
		err               error
		want              bool
	}{
		{"the filesystem's own, kept out by a permission", filepath.Join(root, "lost+found"), "lost+found", denied, true},
		{"EPERM is a permission too", filepath.Join(root, "lost+found"), "lost+found", syscall.EPERM, true},
		{"an I/O error on it is a fault", filepath.Join(root, "lost+found"), "lost+found", syscall.EIO, false},
		{"one level down, a volume mounted inside the tree", filepath.Join(root, "Artist", "lost+found"), "lost+found", denied, false},
		{"another name, however it is locked", filepath.Join(root, "Lost+Found"), "Lost+Found", denied, false},
		{"another name at the top", filepath.Join(root, "Artist"), "Artist", denied, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := isFilesystemLostFound(root, c.path, fakeDirEntry{name: c.entry}, c.err); got != c.want {
				t.Errorf("isFilesystemLostFound(%q, %v) = %v, want %v", c.path, c.err, got, c.want)
			}
		})
	}
}

// TestMassOrphanRefusalForWeighsWhatTheWalkCouldNotStat — an entry the walk
// could not stat is nothing to the sweep, one file a row references, or one
// orphan. Counting every one as an orphan must refuse EXACTLY when some
// reading of them would: checked here against every reading, by brute
// force, over every shape small enough to enumerate, at the thresholds
// that matter (0 refuses any mass, 100 disables the guard). An unlisted
// directory is not weighed here: it is unbounded, PartialWalkRefusal's.
func TestMassOrphanRefusalForWeighsWhatTheWalkCouldNotStat(t *testing.T) {
	checked := 0
	for _, pct := range []int{0, 10, 20, 50, 99, 100} {
		for rows := 0; rows <= 14; rows++ {
			for orphans := 0; orphans <= 14; orphans++ {
				for files := orphans; files <= orphans+10; files++ {
					for k := 0; k <= 4; k++ {
						some := false
						for asOrphans := 0; asOrphans <= k && !some; asOrphans++ {
							for asKnown := 0; asOrphans+asKnown <= k && !some; asKnown++ {
								some = MassOrphanRefusal(orphans+asOrphans, files+asOrphans+asKnown, rows, pct) != ""
							}
						}
						inv := SidecarInventory{Orphans: orphans, Files: files, Unreadable: k}
						if got := MassOrphanRefusalFor(inv, rows, pct) != ""; got != some {
							t.Fatalf("orphans=%d files=%d rows=%d pct=%d with %d unstattable entr(y/ies): refuse=%v, "+
								"but some reading of them refuses=%v", orphans, files, rows, pct, k, got, some)
						}
						checked++
					}
				}
			}
		}
	}
	if checked < 10000 {
		t.Fatalf("checked only %d shapes — the enumeration is broken", checked)
	}

	// The reason says when its numbers are a worst case, and which.
	reason := MassOrphanRefusalFor(SidecarInventory{Orphans: 9, Files: 20, Unreadable: 1}, 5, 20)
	if !strings.Contains(reason, "10 of 21 file(s)") || !strings.Contains(reason, "counting the 1 entr(y/ies) the walk could not stat") {
		t.Errorf("one unstattable link over nine orphans: reason %q, want the worst case named as one", reason)
	}
	// An unlisted directory is not a bounded unknown, so it is not weighed.
	if r := MassOrphanRefusalFor(SidecarInventory{Orphans: 9, Files: 20, Unreadable: 1, UnlistedDirs: 1}, 5, 20); r != "" {
		t.Errorf("an unlisted directory was weighed as one file: %q", r)
	}
	// And without unknowns it is MassOrphanRefusal, word for word.
	if a, b := MassOrphanRefusalFor(SidecarInventory{Orphans: 40, Files: 42}, 2, 20), MassOrphanRefusal(40, 42, 2, 20); a != b || a == "" {
		t.Errorf("with nothing unknown: %q, want MassOrphanRefusal's %q", a, b)
	}
}

// TestPartialWalkRefusalNeedsADirectoryItCouldNotList — the refusal is for
// the unbounded unknown alone, names what it could not read and what it
// counted, and has nothing to protect where the mass-orphan guard is off.
func TestPartialWalkRefusalNeedsADirectoryItCouldNotList(t *testing.T) {
	links := SidecarInventory{Orphans: 15, Files: 35, Unreadable: 2}
	if r := PartialWalkRefusal(links, 20, 20); r != "" {
		t.Errorf("two unstattable links, no unlisted directory: refused with %q", r)
	}
	dirs := SidecarInventory{Orphans: 15, Files: 35, Unreadable: 3, UnlistedDirs: 1}
	r := PartialWalkRefusal(dirs, 20, 20)
	for _, want := range []string{"could not list 1 director(y/ies)", "15 orphan(s) of 35 file(s) against 20 row(s)"} {
		if !strings.Contains(r, want) {
			t.Errorf("reason %q does not say %q", r, want)
		}
	}
	if r := PartialWalkRefusal(dirs, 20, 100); r != "" {
		t.Errorf("at 100%% the mass-orphan check never refuses, so a partial walk hides no refusal; got %q", r)
	}
}

// TestOrphanSidecarSweeperProceedsPastTheFilesystemsLostFound — the
// ordinary host #1063 left refusing: a variants directory that is an ext4
// volume's mount point, with the volume's root-owned lost+found at its top.
// An ordinary crop of orphans there is reclaimed, with no partial-walk
// refusal. (On #1063's code every tick refused, logging the partial-walk
// WARN, and unlinked nothing.)
func TestOrphanSidecarSweeperProceedsPastTheFilesystemsLostFound(t *testing.T) {
	skipWhereModesDenyNothing(t)
	dir := t.TempDir()
	live := seedTestSidecarTree(t, dir, "live-", 20)
	seedTestSidecarTree(t, dir, "orphan-", 15)
	lostFound := filepath.Join(dir, "lost+found")
	seedTestSidecarTree(t, lostFound, "#", 3)
	ageFixtures(t, dir)
	lockDir(t, lostFound)
	s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)
	s.gracePeriodForTest = time.Nanosecond
	rec := loggingtest.Record(t)

	if n := s.tick(context.Background()); n != 15 {
		t.Errorf("unlinked %d, want the 15 orphans beside the filesystem's lost+found", n)
	}
	requireLinesSay(t, rec.Lines(msgOrphanPartialWalk), 0, "no partial-walk refusal over the filesystem's lost+found")
	requireLinesSay(t, rec.Lines(msgOrphanTickComplete), 1, "the tick's summary", " unreadable=0", " refused=false", " unlinked=15")
	if err := os.Chmod(lostFound, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := countFiles(t, lostFound); got != 3 {
		t.Errorf("%d of the 3 files in lost+found survive", got)
	}
}

// TestOrphanSidecarSweeperWeighsALinkItCouldNotStat — a link the walk could
// not stat is at most one file, so it no longer makes the sweep refuse
// every tick as #1063's did: an ordinary crop beside it is reclaimed and
// the link is left. Where that one file could flip the verdict, the tick
// refuses as a mass orphaning, naming the worst case it counted.
func TestOrphanSidecarSweeperWeighsALinkItCouldNotStat(t *testing.T) {
	skipWhereModesDenyNothing(t)
	for _, c := range []struct {
		name            string
		live, orphans   int
		wantUnlinked    int
		wantMassRefusal bool
	}{
		{"an ordinary crop beside it is reclaimed", 20, 15, 15, false},
		{"a link that could be the tenth orphan refuses", 5, 9, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			live := seedTestSidecarTree(t, dir, "live-", c.live)
			seedTestSidecarTree(t, dir, "orphan-", c.orphans)
			ageFixtures(t, dir)
			link := filepath.Join(dir, "Parked", "album.flac")
			linkThroughALockedDir(t, link)
			s := NewOrphanSidecarSweeper(&fakeSidecarLister{known: pathSet(live)}, staticDir(dir), time.Hour, sweepPercent)
			s.gracePeriodForTest = time.Nanosecond
			rec := loggingtest.Record(t)

			if n := s.tick(context.Background()); n != c.wantUnlinked {
				t.Errorf("unlinked %d, want %d", n, c.wantUnlinked)
			}
			requireLinesSay(t, rec.Lines(msgOrphanPartialWalk), 0, "a bounded unknown is no partial walk")
			wantRefusals := 0
			if c.wantMassRefusal {
				wantRefusals = 1
			}
			requireLinesSay(t, rec.Failures(msgOrphanRefusal), wantRefusals, "the mass-orphan refusal",
				fmt.Sprintf("%d of %d file(s)", c.orphans+1, c.live+c.orphans+1), "counting the 1 entr(y/ies) the walk could not stat")
			if _, err := os.Lstat(link); err != nil {
				t.Errorf("the link the walk could not stat is gone: %v", err)
			}
		})
	}
}
