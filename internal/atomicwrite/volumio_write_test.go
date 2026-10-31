package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// volumioWriter stands the strict writer on the measured box with base, a
// directory of the test's, for the root overlay's own root: every directory
// from base down is on the overlay's mount (31) by the mount IDs the writer
// reads, and everything above it on another; the medium check is the
// inspection Linux runs (platformInspect), on the box's mount table, files
// and mount IDs, its statfs giving the overlay for every path below base and
// ENOENT for the upper path /mnt/ext/dyn; and the directories above the root
// a copy-up can create entries in are copyUpsFrom's, on its own statfs, as
// Linux's platformCopyUps reads them. It returns what each statfs was asked.
func volumioWriter(t *testing.T, base string, box *volumioBox) (medium, copyUps *[]string) {
	t.Helper()
	under := func(p string) bool { return p == base || strings.HasPrefix(p, base+string(filepath.Separator)) }
	stubMountIDs(t, func(p string) (uint64, bool, error) {
		if under(p) {
			return 31, true, nil
		}
		return 1, true, nil
	})
	statfsAsked := func(asked *[]string) func(string) (fsStat, error) {
		return func(p string) (fsStat, error) {
			*asked = append(*asked, p)
			if under(p) {
				return piOverlay, nil
			}
			return fsStat{}, &fs.PathError{Op: "statfs", Path: p, Err: syscall.ENOENT}
		}
	}
	medium, copyUps = new([]string), new([]string)
	readers := mediumReaders{
		statfs:    statfsAsked(medium),
		mountinfo: func() ([]byte, error) { return []byte(box.table), nil },
		idOf: func(p string) (uint64, bool, error) {
			id, ok := box.ids[p]
			return id, ok, nil
		},
		boot: box.boot,
	}
	stubMedium(t, func(root string, id uint64) error { return inspectMedium(root, id, readers).Err })
	copyStatfs := statfsAsked(copyUps)
	stubCopyUps(t, func(root string, id uint64) ([]string, error) { return copyUpsFrom(root, id, copyStatfs, mountIDOf) })
	return medium, copyUps
}

// logSyncsAndRenames records, in one log, each directory the strict barrier
// fsyncs and each rename, so a test reads their order.
func logSyncsAndRenames(t *testing.T, fail map[string]error) *[]string {
	t.Helper()
	var log []string
	prevDir := syncDir
	syncDir = func(d *os.File) error {
		log = append(log, d.Name())
		return fail[d.Name()]
	}
	prevRename := SetRenameFuncForTest(func(src, dst string) error {
		log = append(log, "rename")
		return os.Rename(src, dst)
	})
	t.Cleanup(func() {
		syncDir = prevDir
		SetRenameFuncForTest(prevRename)
	})
	return &log
}

// TestWriteBytesDurable_SyncsThroughTheMergedMountWhereVolumiosUpperIsHidden
// pins the durability the strict writer keeps on Volumio, where no path
// reaches the root overlay's upper layer (B603, the orchestrator's
// amendment 5): the write and the probe are admitted by the inspection's
// verdict, and every sync they make is of a directory on the overlay's own
// mount, the merged view, never of the upper path, which the medium check
// alone asks statfs of, once. Before the staging the write syncs the
// directories above the one it creates; once the rename lands, the file's
// directory, the root, which holds the created directory's entry, and every
// directory above the root up to the merged mount's own root, which hold
// the entries a copy-up creates (copyUpsFrom, which reads statfs of the
// root alone and finds the overlay the merged path is). A directory above
// the root whose fsync fails, and one that cannot be opened for its sync,
// fail the write and the probe, naming it, as on any overlay.
func TestWriteBytesDurable_SyncsThroughTheMergedMountWhereVolumiosUpperIsHidden(t *testing.T) {
	base := realTempDir(t)
	mid := filepath.Join(base, "1-bit-bridge")
	root := filepath.Join(mid, "data")
	player := filepath.Join(root, "player")
	mkdirs(t, root)
	box := piVolumio()
	medium, copyUps := volumioWriter(t, base, box)
	log := logSyncsAndRenames(t, nil)
	path := filepath.Join(player, "ids")
	if err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable where Volumio's upper layer is hidden: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "reserved" {
		t.Fatalf("the write left %q, %v", got, err)
	}
	if want := []string{root, "rename", player, root, mid, base}; !slices.Equal(*log, want) {
		t.Errorf("the write synced and renamed %v, want %v: the root before the staging, then once the rename landed the file's directory, the root, and every directory above it up to the merged mount's root", *log, want)
	}
	if want := []string{root, volumioUpperDir}; !slices.Equal(*medium, want) {
		t.Errorf("the medium check asked statfs of %v, want %v: the root, and the upper path once", *medium, want)
	}
	if want := []string{root}; !slices.Equal(*copyUps, want) {
		t.Errorf("the copy-up walk asked statfs of %v, want the root alone, %v", *copyUps, want)
	}
	for _, p := range *log {
		if strings.HasPrefix(p, "/mnt/ext") {
			t.Errorf("the write synced %s, below the upper path", p)
		}
	}
	*log, *medium, *copyUps = nil, nil, nil
	if err := ProbeDurable(root, path); err != nil {
		t.Fatalf("ProbeDurable where Volumio's upper layer is hidden: %v", err)
	}
	if want := []string{player, root, mid, base}; !slices.Equal(*log, want) {
		t.Errorf("the probe synced %v, want %v", *log, want)
	}
	t.Run("an fsync that fails above the root", func(t *testing.T) {
		logSyncsAndRenames(t, map[string]error{base: syscall.EIO})
		for what, err := range map[string]error{
			"WriteBytesDurable": WriteBytesDurable(root, path, []byte("reserved again"), ".durable-*.tmp"),
			"ProbeDurable":      ProbeDurable(root, path),
		} {
			if !errors.Is(err, ErrBarrierFailed) || !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), base+", above the root on an overlay, cannot be synced") {
				t.Errorf("%s = %v, want the barrier's failure, naming %s", what, err, base)
			}
		}
	})
	t.Run("a directory above the root that cannot be opened", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX permission bits")
		}
		if os.Geteuid() == 0 {
			t.Skip("root can read a directory it may not")
		}
		logSyncsAndRenames(t, nil)
		if err := os.Chmod(mid, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(mid, 0o700) })
		for what, err := range map[string]error{
			"WriteBytesDurable": WriteBytesDurable(root, path, []byte("reserved again"), ".durable-*.tmp"),
			"ProbeDurable":      ProbeDurable(root, path),
		} {
			if !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrBarrierFailed) || !strings.Contains(err.Error(), mid+", above the root on an overlay, could not be opened for its sync") {
				t.Errorf("%s = %v, want the open's failure, naming %s", what, err, mid)
			}
		}
	})
}

// TestWriteBytesDurable_RefusesWhereTheBoxDoesNotShowVolumio pins the
// writer's refusal on a box whose upper layer no path reaches and whose own
// files do not show Volumio's layout (here the NVMe's decoy partition, with
// no /imgpart mount): the write and the probe refuse it with the medium's
// refusal, before anything is created, written or synced.
func TestWriteBytesDurable_RefusesWhereTheBoxDoesNotShowVolumio(t *testing.T) {
	base := realTempDir(t)
	root := filepath.Join(base, "data")
	mkdirs(t, root)
	box := piVolumio()
	box.line(26, "")
	box.ids["/imgpart"] = 31
	box.line(110, "")
	volumioWriter(t, base, box)
	log := logSyncsAndRenames(t, nil)
	path := filepath.Join(root, "player", "ids")
	for what, err := range map[string]error{
		"WriteBytesDurable": WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp"),
		"ProbeDurable":      ProbeDurable(root, path),
	} {
		if !errors.Is(err, ErrMediumUnknown) || !strings.Contains(err.Error(), fmt.Sprintf("%s is no mount of its own", volumioImageMount)) {
			t.Errorf("%s = %v, want the medium's refusal", what, err)
		}
	}
	if len(*log) != 0 {
		t.Errorf("the refused write and probe synced or renamed %v", *log)
	}
	if _, err := os.Lstat(filepath.Join(root, "player")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused write made %s (%v)", filepath.Join(root, "player"), err)
	}
}
