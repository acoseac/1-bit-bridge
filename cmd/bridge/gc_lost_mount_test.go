package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// `bridge upscale --gc` asked the same probe as the watcher whether its
// variants directory looked unmounted, and the probe called any directory
// holding an entry healthy (backlog B223). Over a volume unmounted from a
// mountpoint that held a .DS_Store, the relocation pre-flight walked the
// local directory, found no sidecar and passed; the forward sweep unlinked
// the .DS_Store as an orphan; and the reverse guard then read the directory
// as one this run had emptied, and deleted every row. With the folders a
// failed render leaves, nothing needed explaining: the directory was
// "healthy". The run now refuses before it touches anything.

// lostMountGCTree is 40 rows whose renditions sit on the "volume" mounted
// at dir, with the volume then unmounted: moved aside, and a new local
// directory at dir holding the junk named by files (relative paths) and
// the directories named by dirs.
func lostMountGCTree(t *testing.T, files, dirs []string) (rows func() int, dir string, runGCHere func(opts gcOptions) (int, string)) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "mnt", "variants")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := strandedTree(t, dir, 40, 0)
	if err := atomicwrite.RenameWithRetry(dir, dir+".volume"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		writeFixtureFile(t, filepath.Join(dir, filepath.FromSlash(f)), 10)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rows = func() int {
		t.Helper()
		all, err := st.AllVariants(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(all)
	}
	return rows, dir, func(opts gcOptions) (int, string) {
		var stdout, stderr bytes.Buffer
		rc := runGC(context.Background(), &stdout, &stderr, st, dir, t.TempDir(), opts)
		return rc, stdout.String() + stderr.String()
	}
}

// TestRunGCRefusesAnUnmountedVolumeWhoseMountpointHoldsNoRendition — the
// backlog's second reproduction, through the real runGC: on main each
// shape exited 0 having deleted all forty rows. The run now refuses before
// the forward sweep, so the junk in the local directory is untouched too,
// and it names the way past for a tree whose renditions really are gone.
func TestRunGCRefusesAnUnmountedVolumeWhoseMountpointHoldsNoRendition(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		dirs  []string
	}{
		{name: "a .DS_Store", files: []string{".DS_Store"}},
		{name: "the folders a failed render left", dirs: []string{"Artist/Re-rendered"}},
		{name: "a .flac that is not a rendition", files: []string{"Artist/Re-rendered/00.flac"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, dir, gc := lostMountGCTree(t, tc.files, tc.dirs)
			rc, out := gc(gcOptions{maxDeletePercent: 20})
			if rc == 0 {
				t.Fatalf("--gc over an unmounted volume whose mountpoint holds %s exited 0\n%s", tc.name, out)
			}
			if n := rows(); n != 40 {
				t.Errorf("%d row(s) left of 40 after a refused --gc\n%s", n, out)
			}
			for _, f := range tc.files {
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
					t.Errorf("a refused --gc removed %s from the mountpoint: %v", f, err)
				}
			}
			for _, want := range []string{"holds no rendition", "refusing to delete rows", "--allow-mass-delete"} {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestRunGCReapsATreeWhoseRenditionsWereDeletedByHandWhenAllowed — the
// case the refusal costs: every rendition deleted by hand, the folders left
// behind, or the directory emptied outright. It looks exactly like an
// unmounted volume, so the run refuses it; --allow-mass-delete is the
// operator saying the renditions really are gone, and the rows go. The
// emptied-outright half had no way past at all before (CLAUDE.md, #941).
func TestRunGCReapsATreeWhoseRenditionsWereDeletedByHandWhenAllowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		dirs []string
	}{
		{name: "the folders left behind", dirs: []string{"Artist/Re-rendered"}},
		{name: "the directory emptied outright"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, _, gc := lostMountGCTree(t, nil, tc.dirs)
			if rc, out := gc(gcOptions{maxDeletePercent: 20}); rc == 0 || rows() != 40 {
				t.Fatalf("without the flag: rc=%d, %d row(s); want a refusal keeping all 40\n%s", rc, rows(), out)
			}
			if rc, out := gc(gcOptions{maxDeletePercent: 20, allowMassDelete: true}); rc != 0 {
				t.Fatalf("with --allow-mass-delete: rc=%d, want 0\n%s", rc, out)
			}
			if n := rows(); n != 0 {
				t.Errorf("with --allow-mass-delete %d row(s) survived, want 0", n)
			}
		})
	}
}

// TestTheReverseGuardCountsOnlyTheRenditionsThisRunUnlinked — the reverse
// guard's Empty exception reads the renditions the forward sweep's own
// os.Remove unlinked, never the renditions the inventory listed
// (CodeRabbit on #1129): a volume unmounted between the inventory and the
// unlinks makes every unlink ENOENT, a success to the sweep's exit code,
// over a mountpoint that holds no rendition, and the listed count read
// that as this run's own work, so the reverse sweep went on to delete the
// rows. Stood in by the three steps runGC takes, with the unmount between
// the inventory and the forward sweep.
func TestTheReverseGuardCountsOnlyTheRenditionsThisRunUnlinked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mnt", "variants")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, stranded := strandedTree(t, dir, 0, 12)
	var stderr bytes.Buffer
	inv, code := gcTakeInventory(context.Background(), &stderr, dir, map[string]struct{}{})
	if code != 0 || len(inv.OrphanPaths) != len(stranded) {
		t.Fatalf("inventory: code %d, %d orphans, want %d\n%s", code, len(inv.OrphanPaths), len(stranded), stderr.String())
	}
	if err := atomicwrite.RenameWithRetry(dir, dir+".volume"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	removed, _, failed, exitCode, unlinked := runGCForwardSweep(context.Background(), &bytes.Buffer{}, &stderr, inv, gcStartAfterTheGrace())
	if exitCode != 0 || failed != 0 || removed != len(stranded) {
		t.Fatalf("forward sweep: exit %d, %d removed, %d failed; want every unlink of a file already gone read as removed", exitCode, removed, failed)
	}
	if unlinked != 0 {
		t.Errorf("the sweep counted %d renditions unlinked; every one was gone before its unlink", unlinked)
	}
	if rc := gcCheckOutputDirBeforeReverseSweep(&stderr, dir, 40, unlinked, false); rc == 0 {
		t.Errorf("the reverse guard passed a mountpoint holding no rendition that this run emptied of nothing\n%s", stderr.String())
	}
}

// TestTheForwardSweepCountsARenditionOnlyWhenItUnlinkedIt — of four
// orphans, a rendition it unlinks counts, a rendition already gone
// (ENOENT) and one whose unlink fails (a directory this user may not
// write) do not, and a file that is not a rendition never does.
func TestTheForwardSweepCountsARenditionOnlyWhenItUnlinkedIt(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory whose mode refuses this user an unlink")
	}
	dir := t.TempDir()
	const variant = "upscaled-v2-176400-24"
	unlinkable := transcode.VariantSidecarPath(dir, "A/01.flac", variant)
	gone := transcode.VariantSidecarPath(dir, "B/01.flac", variant)
	locked := transcode.VariantSidecarPath(dir, "C/01.flac", variant)
	junk := filepath.Join(dir, "A", "notes.txt")
	for _, p := range []string{unlinkable, gone, locked, junk} {
		writeFixtureFile(t, p, 10)
	}
	var stderr bytes.Buffer
	inv, code := gcTakeInventory(context.Background(), &stderr, dir, map[string]struct{}{})
	if code != 0 || len(inv.OrphanPaths) != 4 {
		t.Fatalf("inventory: code %d, %d orphans, want 4\n%s", code, len(inv.OrphanPaths), stderr.String())
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	lockedDir := filepath.Dir(locked)
	if err := os.Chmod(lockedDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) })
	removed, _, failed, exitCode, unlinked := runGCForwardSweep(context.Background(), &bytes.Buffer{}, &stderr, inv, gcStartAfterTheGrace())
	if exitCode != 0 || removed != 3 || failed != 1 {
		t.Fatalf("forward sweep: exit %d, %d removed, %d failed; want 0, 3 (the unlinked two and the one already gone), 1\n%s", exitCode, removed, failed, stderr.String())
	}
	if unlinked != 1 {
		t.Errorf("renditions unlinked = %d, want 1: only the rendition this run removed", unlinked)
	}
}
