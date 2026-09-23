//go:build !windows

package updater

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// swapFallbacks drives swapBinary over the four combinations of the two
// independent fallbacks, which swap_test.go covers only one at a time.
//
// The composed case is the one that mattered: no hardlink support pushes
// the swap into the two-rename path, which vacates dst, and EXDEV then
// made the install step a ~30 MiB copy plus an fsync WITH DST ABSENT.
// Each fallback alone is benign — the hardlink path keeps dst resolving
// through its own dentry for the whole copy, and a same-volume
// two-rename swap really is two adjacent renames — so no test of either
// on its own could see it.
type swapFallbacks struct {
	name        string
	noHardlink  bool
	crossDevice bool
}

// installFallbackStubs wires the requested failure modes and returns a
// function yielding the ordered trace of the two events that matter.
//
// What is asserted is NOT "dst is never absent" — the two-rename commit
// has an irreducible gap between `rename(dst, bak)` and
// `rename(staged, dst)`, and that gap is the intended residual. The
// property is that the EXPENSIVE step does not happen inside it: the
// cross-volume copy must run while dst is still present.
//
// The trace records "copy" at the moment the stub returns EXDEV, because
// the copy follows that return directly, and "vacate" when dst is
// renamed to .bak. The fix is exactly "copy before vacate", so that is
// what the test says.
func installFallbackStubs(t *testing.T, fb swapFallbacks, dst string) func() []string {
	t.Helper()
	var trace []string
	dstGoneAtCopy := false

	if fb.noHardlink {
		origLink := linkFunc
		linkFunc = func(string, string) error {
			return errors.New("simulated link-unsupported filesystem")
		}
		t.Cleanup(func() { linkFunc = origLink })
	}

	origRename := renameFunc
	renameFunc = func(oldname, newname string) error {
		// Only the staging move can legitimately be cross-device; the
		// vacate is dst -> dst.bak, one directory, and returning EXDEV
		// for it would model a filesystem that cannot exist.
		if fb.crossDevice && filepath.Dir(oldname) != filepath.Dir(newname) {
			trace = append(trace, "copy")
			if _, err := os.Stat(dst); os.IsNotExist(err) {
				dstGoneAtCopy = true
			}
			return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: syscall.EXDEV}
		}
		if filepath.Ext(newname) == ".bak" {
			trace = append(trace, "vacate")
		}
		return os.Rename(oldname, newname)
	}
	t.Cleanup(func() { renameFunc = origRename })

	return func() []string {
		if dstGoneAtCopy {
			return append(append([]string{}, trace...), "dst-absent-during-copy")
		}
		return trace
	}
}

// TestSwapBinaryKeepsDstPresentAcrossEveryFallbackCombination is the
// composed-fallback pin: whichever pair of fallbacks fires, the install
// lands, the rollback target survives, and dst is never observed absent
// while the expensive work is happening.
func TestSwapBinaryKeepsDstPresentAcrossEveryFallbackCombination(t *testing.T) {
	for _, fb := range []swapFallbacks{
		{name: "hardlink and same volume", noHardlink: false, crossDevice: false},
		{name: "no hardlink, same volume", noHardlink: true, crossDevice: false},
		{name: "hardlink, cross volume", noHardlink: false, crossDevice: true},
		{name: "no hardlink, cross volume", noHardlink: true, crossDevice: true},
	} {
		t.Run(fb.name, func(t *testing.T) {
			// The staged binary lives in its own directory, as the real
			// scratch dir does — otherwise "cross-device" cannot be
			// modelled by a directory comparison.
			dir := t.TempDir()
			scratch := t.TempDir()
			live := fakeBinary(t, dir, "bridge", "OLD")
			newBin := fakeBinary(t, scratch, "bridge", "NEW")

			traceOf := installFallbackStubs(t, fb, live)

			if err := swapBinary(live, newBin, ".bak", nil); err != nil {
				t.Fatalf("swapBinary: %v", err)
			}
			trace := traceOf()
			if got, _ := os.ReadFile(live); string(got) != "NEW" {
				t.Errorf("live binary = %q, want NEW", string(got))
			}
			if bak, _ := os.ReadFile(live + ".bak"); string(bak) != "OLD" {
				t.Errorf(".bak = %q, want OLD (the rollback contract)", string(bak))
			}
			for _, ev := range trace {
				if ev == "dst-absent-during-copy" {
					t.Error("dst was absent while the cross-volume copy ran — the expensive step is still inside the no-file window")
				}
			}
			// The ordering is the fix. A copy recorded after a vacate is
			// the defect; a copy with no vacate before it is correct on
			// both the hardlink and the two-rename paths.
			if ci, vi := indexOf(trace, "copy"), indexOf(trace, "vacate"); ci >= 0 && vi >= 0 && ci > vi {
				t.Errorf("trace %v: the cross-volume copy ran AFTER dst was vacated", trace)
			}
			if fb.crossDevice && indexOf(trace, "copy") < 0 {
				t.Errorf("trace %v: the cross-device branch never fired; the test is proving nothing", trace)
			}
			if fb.noHardlink && indexOf(trace, "vacate") < 0 {
				t.Errorf("trace %v: the two-rename path never fired; the test is proving nothing", trace)
			}
			// The scratch copy is consumed either way — a swap that
			// leaves it behind fills the data dir over time.
			if _, err := os.Stat(newBin); !os.IsNotExist(err) {
				t.Errorf("staged source survived the swap; stat err = %v", err)
			}
			// No staging temp left beside the binary.
			ents, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ents {
				if filepath.Ext(e.Name()) != ".bak" && e.Name() != "bridge" {
					t.Errorf("swap left %q beside the binary", e.Name())
				}
			}
		})
	}
}

// TestSwapBinaryPreservesTheInstalledMode pins B2.
//
// The rename path inherited the extractor's `O_CREATE 0o755`, which IS
// umask-masked, while the copy path chmod'd an unmasked 0o755 — under a
// comment claiming the two matched. Under `UMask=0027`, which this
// repo's own deployment runbook prescribes, an update silently took the
// binary from 0755 to 0750: the service user still execs it, so nothing
// breaks visibly, and every other account on the host gets EACCES.
//
// The divergence is invisible at umask 0, so the test sets one.
func TestSwapBinaryPreservesTheInstalledMode(t *testing.T) {
	for _, fb := range []swapFallbacks{
		{name: "same volume", crossDevice: false},
		{name: "cross volume", crossDevice: true},
	} {
		t.Run(fb.name, func(t *testing.T) {
			old := syscall.Umask(0o027)
			t.Cleanup(func() { syscall.Umask(old) })

			dir := t.TempDir()
			scratch := t.TempDir()
			live := fakeBinary(t, dir, "bridge", "OLD")
			newBin := fakeBinary(t, scratch, "bridge", "NEW")
			// The mode the operator's install actually has.
			if err := os.Chmod(live, 0o755); err != nil {
				t.Fatal(err)
			}
			_ = installFallbackStubs(t, fb, live)

			if err := swapBinary(live, newBin, ".bak", nil); err != nil {
				t.Fatalf("swapBinary: %v", err)
			}
			fi, err := os.Stat(live)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != 0o755 {
				t.Errorf("installed mode = %#o, want %#o — an update must not change the binary's permissions", got, 0o755)
			}
		})
	}
}

// indexOf is the first position of want in trace, or -1.
func indexOf(trace []string, want string) int {
	for i, e := range trace {
		if e == want {
			return i
		}
	}
	return -1
}
