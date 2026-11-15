package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// swapByVacate is the filesystem part of Windows' swap (swapBinary, which
// stops the SCM service first): it puts newBinary at dst and keeps what was
// there at dst+backupExt, by renames alone, with no SCM in it, so every host's
// tests run it.
//
// Windows lets the running image be renamed and refuses to replace it in
// place, so dst is vacated to bak first and the new binary renamed into the
// emptied path; the no-file window between those two renames is structurally
// unavoidable there. They are adjacent renames in one directory: the candidate
// is beside dst before anything moves (the settings check's copy, which every
// production install stages there, or a staging of its own here), so the
// window never encloses a copy. NTFS journals each rename, and the boot's
// marker and the SCM's restart recover a crash in it.
//
// Nothing waits once anything is armed (backlog B898): the staging, where
// there is one, and the probes of the candidate, of dst and of a bak already
// there (a scanner holding the candidate it has just seen run for its settings
// check) share one forward deadline, set as the swap begins, before
// markSwapStarted, and a swap whose probes ended past it arms nothing
// (ErrDeadlinePassed); the vacate and the place then have one attempt each,
// so a vacate refused after arming moved nothing, and the install's marker,
// put back as it was read, disarms the swap. A refused place is put back on
// the recovery schedule. Each failure is a *SwapError saying what it left.
//
// markSwapStarted (nil in tests) runs immediately before the first rename and
// aborts the swap on error; see State.SwapStarted.
func swapByVacate(dst, newBinary, backupExt string, markSwapStarted func() error) error {
	return liveMoves().swap(dst, newBinary, backupExt, markSwapStarted)
}

// swap is swapByVacate through m.
func (m moves) swap(dst, newBinary, backupExt string, markSwapStarted func() error) error {
	bak := dst + backupExt
	fwd := m.deadlineOf(forwardWaits)
	staged := newBinary
	if !checkedBeside(newBinary, dst) {
		// mode 0: NTFS has no executable bit to set.
		s, err := m.stage(newBinary, dst, 0, ".bridge-swap-*", fwd)
		if err != nil {
			return err
		}
		staged = s
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(staged)
		}
	}()
	if r := m.probeStep(staged, fwd); r.err != nil {
		return failed(NothingMoved, r, fmt.Sprintf("install %s -> %s: %v; nothing was moved", staged, dst, r.err))
	}
	if r := m.probeStep(dst, fwd); r.err != nil {
		return failed(NothingMoved, r, fmt.Sprintf("rename %s -> %s: %v; nothing was moved", dst, bak, r.err))
	}
	if r := m.probeIfThere(bak, fwd); r.err != nil {
		return failed(NothingMoved, r, fmt.Sprintf("rename %s -> %s: %v; nothing was moved", dst, bak, r.err))
	}
	if m.passed(fwd) {
		return failed(NothingMoved, stepResult{err: ErrDeadlinePassed},
			fmt.Sprintf("rename %s -> %s: %v; nothing was moved", dst, bak, ErrDeadlinePassed))
	}
	if err := armSwap(markSwapStarted); err != nil {
		return err
	}
	if err := m.commit(dst, staged, bak); err != nil {
		return err
	}
	committed = true
	return nil
}

// commit makes the swap's two adjacent renames, dst to bak and then staged to
// dst, one attempt each whatever the time, and where the place is refused
// puts bak back on the recovery schedule (putBack): the Windows swap's, and
// the Unix swap's on a filesystem that cannot hardlink (commitViaRename).
func (m moves) commit(dst, staged, bak string) error {
	if r := m.once(StageVacate, dst, bak); r.err != nil {
		return failed(NothingMoved, r, fmt.Sprintf("rename %s -> %s: %v; nothing was moved", dst, bak, r.err))
	}
	r := m.once(StagePlace, staged, dst)
	if r.err == nil {
		return nil
	}
	p := m.putBack(bak, dst)
	if p.err != nil {
		e := failed(BinaryMissing, p, fmt.Sprintf("install %s -> %s failed (%v); rollback also failed (%v); "+
			"manual recovery needed: rename %s to %s, then start the bridge", staged, dst, r.err, p.err, bak, dst))
		e.Recover, e.errs = fmt.Sprintf("rename %s to %s", bak, dst), []error{r.err, p.err}
		return e
	}
	return failed(PutBack, r, fmt.Sprintf("install %s -> %s: %v (rolled back)", staged, dst, r.err))
}

// checkStagePrefix begins the name of the candidate an install stages beside
// the binary for its settings check (stageForCheck).
const checkStagePrefix = ".bridge-check-"

// checkedBeside reports whether newBinary is that candidate, already beside
// dst: the swap takes it as its staged file and stages nothing of its own. A
// file of any other name is staged, so a swap's caller never has its own file
// moved into place.
func checkedBeside(newBinary, dst string) bool {
	return filepath.Dir(newBinary) == filepath.Dir(dst) && strings.HasPrefix(filepath.Base(newBinary), checkStagePrefix)
}
