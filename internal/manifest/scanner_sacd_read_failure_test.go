package manifest

// The scanner half of the SACD read-failure rule. processSACDISO retires
// every virtual row a container's expansion no longer mints, at threshold 1
// and with a tombstone to every paired device, so:
//
//   - a read that did not complete retires nothing and writes nothing;
//   - a container that changed during the scan is left for the next one;
//   - a container read whole that stopped being an SACD still retires its
//     rows, with tombstones (the positive control: the assertions below can
//     see a retire when one happens).
//
// The parser half is in sacd_read_failure_test.go.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// Messages processSACDISO logs.
const (
	msgSACDExpand   = "sacd expand"
	msgSACDInMotion = "sacd container changed during the scan; left for the next one"
	msgSACDRetired  = "sacd retired stale virtual rows"
)

// failingISO is an opened container whose reads fail where its faults say,
// the way a NAS's reads do. Stat and Close are the real file's, so the
// in-motion guard sees the file the expansion read.
type failingISO struct {
	*os.File
	faults []sacdFault
	failed *atomic.Int64
}

func (f failingISO) ReadAt(p []byte, off int64) (int, error) {
	if fault, ok := sacdFaultAt(f.faults, off, len(p)); ok {
		f.failed.Add(1)
		return 0, &os.PathError{Op: "read", Path: f.Name(), Err: fault.err}
	}
	return f.File.ReadAt(p, off)
}

// installSACDOpener makes sc open every `.iso` container through wrap.
func installSACDOpener(sc *Scanner, wrap func(abs string, f *os.File) sacdContainer) {
	sc.openSACD = func(abs string) (sacdContainer, error) {
		f, err := os.Open(abs)
		if err != nil {
			return nil, err
		}
		return wrap(abs, f), nil
	}
}

// sacdStampedThenTouched indexes a two-track image, then moves its mtime so
// the next scan's skip gate re-expands it. It returns the store, the scanner,
// the container's path, the mtime the initial scan stamped on the rows, and
// the one the next walk will see.
func sacdStampedThenTouched(t *testing.T) (*Store, *Scanner, string, time.Time, time.Time) {
	t.Helper()
	root, store, sc := sacdScanFixture(t)
	iso := writeSACDFixture(t, filepath.Join(root, "Music"), "Album.iso",
		twoFixtureTracks(), sacdFixtureOptions{})
	scanOnce(t, sc, "initial")
	if rows := sacdTrackPathsUnder(t, store, "Music/Album.iso"); len(rows) != 2 {
		t.Fatalf("precondition: %v", rows)
	}
	first, err := store.GetTrack(context.Background(), "Music/Album.iso/st/01.dff")
	if err != nil || first == nil {
		t.Fatalf("precondition: %v", err)
	}
	touched := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(iso, touched, touched); err != nil {
		t.Fatal(err)
	}
	return store, sc, iso, first.ModTime, touched
}

// sacdTombstones returns every deletion the store has journaled and would
// hand a delta client. It asks from the epoch, not from a time.Now() taken
// before the rescan: the initial scan journals nothing, so every tombstone
// here is the rescan's, and a Windows clock tick cannot put the retire in
// the same instant as the query's bound (DeletedSince is strictly after).
func sacdTombstones(t *testing.T, store *Store) []string {
	t.Helper()
	deleted, overflow, err := store.DeletedSince(context.Background(), time.Unix(0, 0))
	if err != nil || overflow {
		t.Fatalf("DeletedSince: overflow=%v err=%v", overflow, err)
	}
	return deleted
}

// requireSACDRowsUntouched asserts that the rescan retired nothing, journaled
// nothing and rewrote nothing: both rows remain, no deletion was journaled
// (counted in the table, since DeletedSince hides a tombstone whose path a
// row still serves), and the first row carries the initial scan's mtime.
func requireSACDRowsUntouched(t *testing.T, store *Store, stamped time.Time) {
	t.Helper()
	if rows := sacdTrackPathsUnder(t, store, "Music/Album.iso"); len(rows) != 2 {
		t.Fatalf("the rows were retired: %v", rows)
	}
	if deleted := sacdTombstones(t, store); len(deleted) != 0 {
		t.Fatalf("tombstones were journaled for rows that remain: %v", deleted)
	}
	var journaled int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM manifest_deletions`).Scan(&journaled); err != nil {
		t.Fatal(err)
	}
	if journaled != 0 {
		t.Fatalf("%d deletions were journaled", journaled)
	}
	first, err := store.GetTrack(context.Background(), "Music/Album.iso/st/01.dff")
	if err != nil || first == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if !first.ModTime.Equal(stamped) {
		t.Fatalf("the row was rewritten: mtime %v, the initial scan stamped %v", first.ModTime, stamped)
	}
}

// TestScanner_SACDReadFailure_RetiresNothing is the defect: a read error at
// any of the three sites that dropped it turned into "not an SACD", and the
// album's rows were deleted and tombstoned while the file sat on disk. The
// skip gate re-enters this path on any change to the container's size or
// mtime and on every ExtractorVersion bump, so a flaky NAS mount could do it
// on any of those scans.
func TestScanner_SACDReadFailure_RetiresNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		faults []sacdFault
	}{
		{"every master-signature probe", sacdProbeFaults(0, syscall.EIO)},
		{"both copies of the area TOC", []sacdFault{
			sacdSectorFault(fixAreaStart, fixTOCSectors, syscall.EIO),
			sacdSectorFault(fixAreaEnd, fixTOCSectors, syscall.EIO),
		}},
		{"the DST probe", []sacdFault{
			{from: fixAudioStart * sacdSectorPayload, to: fixAudioStart*sacdSectorPayload + 1, err: syscall.EIO},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, sc, iso, stamped, _ := sacdStampedThenTouched(t)
			var failed atomic.Int64
			installSACDOpener(sc, func(_ string, f *os.File) sacdContainer {
				return failingISO{File: f, faults: tc.faults, failed: &failed}
			})
			rec := loggingtest.Record(t)
			scanOnce(t, sc, "rescan with failing reads")

			if failed.Load() == 0 {
				t.Fatal("no read reached a fault, so the case tests nothing")
			}
			requireSACDRowsUntouched(t, store, stamped)

			// The failure is reported once, library-relative: an *os.File's
			// read error names the absolute path, and the line rewrites it.
			lines := rec.Failures(msgSACDExpand)
			if len(lines) != 1 {
				t.Fatalf("the read failure was logged %d times, want once:\n%s",
					len(lines), strings.Join(lines, "\n"))
			}
			if !strings.Contains(lines[0], "path=Music/Album.iso ") ||
				!strings.Contains(lines[0], "read Music/Album.iso:") {
				t.Errorf("the line does not name the container library-relative: %s", lines[0])
			}
			if strings.Contains(lines[0], iso) {
				t.Errorf("the line names the container's absolute path: %s", lines[0])
			}
		})
	}
}

// movingISO is an opened container a writer is still writing, as cp over it,
// a download to its final name or a NAS sync leaves it: its bytes end at cut,
// so a COMPLETED read of it answers "not an SACD". move, run once at the
// first read, is the writer's next write.
type movingISO struct {
	*os.File
	cut  int64
	move func()
	once *sync.Once
}

func (m movingISO) ReadAt(p []byte, off int64) (int, error) {
	m.once.Do(m.move)
	if off >= m.cut {
		return 0, io.EOF
	}
	want := len(p)
	if rest := m.cut - off; int64(want) > rest {
		want = int(rest)
	}
	n, err := m.File.ReadAt(p[:want], off)
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

// sacdMove is one way a writer moves a container: it runs in a scanner worker,
// so it reports a failure by returning it rather than through the test.
type sacdMove func(iso, stageDir string, touched time.Time) error

// sacdTouchLater moves the container's mtime past the one the walk saw.
func sacdTouchLater(iso, _ string, touched time.Time) error {
	later := touched.Add(time.Hour)
	return os.Chtimes(iso, later, later)
}

// sacdReplaceByRename renames a copy of the container over it, with the size
// and mtime the walk saw, so that only its identity tells the two apart.
func sacdReplaceByRename(iso, stageDir string, touched time.Time) error {
	body, err := os.ReadFile(iso)
	if err != nil {
		return err
	}
	staged := filepath.Join(stageDir, "Album.iso")
	if err := os.WriteFile(staged, body, 0o644); err != nil {
		return err
	}
	if err := os.Chtimes(staged, touched, touched); err != nil {
		return err
	}
	return os.Rename(staged, iso)
}

// TestScanner_SACDContainerChangingDuringTheScan_KeepsItsRows pins the
// in-motion guard. A container being written in place reads as a file that
// ends early, which is a completed read and an honest "not an SACD" about the
// bytes seen, and without the guard it retires the album at threshold 1. Each
// case moves the file in the way one arm of the guard exists to catch, and
// the logged change names that arm.
func TestScanner_SACDContainerChangingDuringTheScan_KeepsItsRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		// atOpen runs in the opener, before the expansion stats its handle;
		// atRead runs at the expansion's first read. One of them is set.
		atOpen, atRead sacdMove
		want           string
	}{
		{
			// A write lands during the read: the handle's stat and the
			// path's disagree.
			name:   "written while it was read",
			atRead: sacdTouchLater,
			want:   sacdChangedDuringRead,
		},
		{
			// The copy truncated the file after the walk and was idle while
			// it was read: only the walk's stat still shows the file it had.
			name:   "written after the walk, idle while it was read",
			atOpen: sacdTouchLater,
			want:   sacdChangedSinceWalk,
		},
		{
			// A finished copy is renamed over the path while the expansion
			// reads the old file: same size, same mtime, another file.
			name:   "replaced by a rename while it was read",
			atRead: sacdReplaceByRename,
			want:   sacdChangedIdentity,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == sacdChangedIdentity && runtime.GOOS == "windows" {
				// os.Open shares no DELETE access on Windows, so a rename over
				// a file the expansion holds open is refused there.
				t.Skip("Windows refuses a rename over a file the expansion holds open")
			}
			store, sc, _, stamped, touched := sacdStampedThenTouched(t)
			stageDir := t.TempDir()
			var (
				moved   atomic.Int64
				moveErr atomic.Value
			)
			run := func(m sacdMove, iso string) {
				if err := m(iso, stageDir, touched); err != nil {
					moveErr.Store(err)
					return
				}
				moved.Add(1)
			}
			installSACDOpener(sc, func(abs string, f *os.File) sacdContainer {
				if tc.atOpen != nil {
					run(tc.atOpen, abs)
				}
				move := func() {}
				if tc.atRead != nil {
					move = func() { run(tc.atRead, abs) }
				}
				return movingISO{File: f, cut: 1024, move: move, once: new(sync.Once)}
			})
			rec := loggingtest.Record(t)
			scanOnce(t, sc, "rescan of a container in motion")

			if err, _ := moveErr.Load().(error); err != nil {
				t.Fatalf("moving the container: %v", err)
			}
			if moved.Load() != 1 {
				t.Fatalf("the container moved %d times, want once", moved.Load())
			}
			requireSACDRowsUntouched(t, store, stamped)
			lines := rec.Lines(msgSACDInMotion)
			if len(lines) != 1 || !strings.Contains(lines[0], "path=Music/Album.iso change="+tc.want) {
				t.Fatalf("the skip was not logged once as %q:\n%s", tc.want, strings.Join(lines, "\n"))
			}
		})
	}
}

// TestSACDContainerChange drives every arm of the in-motion guard's decision
// alone, with real stats: os.SameFile answers only for the platform's own
// FileInfo, so the identities come from files on disk. Each "moved" case
// differs in one field of one pair, so an arm that stopped comparing that
// field is the one that goes red.
func TestSACDContainerChange(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.iso")
	b := filepath.Join(dir, "b.iso")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("container"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stat := func(p string) os.FileInfo {
		t.Helper()
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	// b: another file with a's size and mtime, so only identity tells them
	// apart.
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, p := range []string{a, b} {
		if err := os.Chtimes(p, t0, t0); err != nil {
			t.Fatal(err)
		}
	}
	a0 := stat(a)
	fb := stat(b)
	// a1: the same file, a later mtime.
	if err := os.Chtimes(a, t0.Add(time.Minute), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	a1 := stat(a)
	// a2: the same file, one byte longer, the first mtime back.
	if err := os.WriteFile(a, []byte("container!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(a, t0, t0); err != nil {
		t.Fatal(err)
	}
	a2 := stat(a)
	if a2.Size() == a0.Size() || !a2.ModTime().Equal(a0.ModTime()) || a1.ModTime().Equal(a0.ModTime()) {
		t.Fatalf("fixture: a0=%d@%v a1=%d@%v a2=%d@%v", a0.Size(), a0.ModTime(),
			a1.Size(), a1.ModTime(), a2.Size(), a2.ModTime())
	}

	for _, tc := range []struct {
		name                      string
		walk, opened, post, lpost os.FileInfo
		want                      string
	}{
		{"nothing moved", a0, a0, a0, a0, ""},
		{"another file at the path", a0, fb, a0, a0, sacdChangedIdentity},
		{"mtime moved while it was read", a1, a0, a1, a1, sacdChangedDuringRead},
		{"size moved while it was read", a2, a0, a2, a2, sacdChangedDuringRead},
		{"mtime moved since the walk", a0, a1, a1, a1, sacdChangedSinceWalk},
		{"size moved since the walk", a0, a2, a2, a2, sacdChangedSinceWalk},
	} {
		if got := sacdContainerChange(tc.walk, tc.opened, tc.post, tc.lpost); got != tc.want {
			t.Errorf("%s: change = %q, want %q", tc.name, got, tc.want)
		}
	}

	// A symlinked container: the walk's stat and the lstat are the link's,
	// the handle's stat and the stat are the target's, and nothing moved.
	link := filepath.Join(dir, "link.iso")
	if err := os.Symlink(b, link); err != nil {
		t.Logf("no symlink case on this host: %v", err)
		return
	}
	lw, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if got := sacdContainerChange(lw, stat(b), stat(link), lw); got != "" {
		t.Errorf("a symlinked container that did not move reads as %q", got)
	}
}

// TestScanner_SACDReadWholeAsJunk_StillRetiresWithTombstones is the positive
// control. A container read to completion that is no longer an SACD retires
// its rows at once, journaled, exactly as before the rule: the rule is about
// reads that did not complete, and the guard about files that moved, and
// neither may cost this.
func TestScanner_SACDReadWholeAsJunk_StillRetiresWithTombstones(t *testing.T) {
	store, sc, iso, _, touched := sacdStampedThenTouched(t)
	if err := os.WriteFile(iso, []byte(strings.Repeat("Z", 2_000_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(iso, touched, touched); err != nil {
		t.Fatal(err)
	}
	rec := loggingtest.Record(t)
	scanOnce(t, sc, "rescan of a container that stopped being an SACD")

	if rows := sacdTrackPathsUnder(t, store, "Music/Album.iso"); len(rows) != 0 {
		t.Fatalf("a completed read of a non-SACD image must retire its rows: %v", rows)
	}
	deleted := sacdTombstones(t, store)
	if len(deleted) != 2 || deleted[0] != "Music/Album.iso/st/01.dff" || deleted[1] != "Music/Album.iso/st/02.dff" {
		t.Fatalf("the retire must reach delta clients as tombstones: %v", deleted)
	}
	if got := rec.Lines(msgSACDRetired); len(got) != 1 {
		t.Errorf("the retire was logged %d times, want once: %v", len(got), got)
	}
	if got := append(rec.Lines(msgSACDExpand), rec.Lines(msgSACDInMotion)...); len(got) != 0 {
		t.Errorf("a stable container read whole was reported as failed or moving: %v", got)
	}
}

// TestScanner_SACDSymlinkedContainer_Expands pins why the guard compares the
// walk's stat with an LSTAT. The walk's stat of a symlinked container is the
// link's own, while the expansion reads the target, so a guard comparing it
// with a stat would read every symlinked container as moved and never expand
// one.
func TestScanner_SACDSymlinkedContainer_Expands(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	target := writeSACDFixture(t, t.TempDir(), "Album.iso", twoFixtureTracks(), sacdFixtureOptions{})
	if err := os.Symlink(target, filepath.Join(root, "Music", "Album.iso")); err != nil {
		t.Skipf("this host cannot create a symlink: %v", err)
	}
	rec := loggingtest.Record(t)
	scanOnce(t, sc, "initial")

	if rows := sacdTrackPathsUnder(t, store, "Music/Album.iso"); len(rows) != 2 {
		t.Fatalf("a symlinked container must expand: %v\n%s", rows,
			strings.Join(append(rec.Lines(msgSACDInMotion), rec.Lines(msgSACDExpand)...), "\n"))
	}
}
