package updater

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// replaceRunningBinary puts bak's bytes at dst without replacing a
// mapped running image in place.
//
// Windows allows a rename of the running executable to a name that is
// not itself mapped, and refuses a rename whose destination is that
// image (a sharing violation, reported as "Access is denied"). Every
// rollback runs in the process that maps dst (RollbackBinary stops no
// service), so a single rename of bak onto dst is the rename the
// kernel refuses. swapBinary already
// vacates the running image before it places the new bytes; this is
// that order for the rollback.
//
// The vacated image is not given the backup extension. canRollback
// treats that name as the previous version, and this file is the
// version being replaced. A second rollback would then restore it.
// Windows will not delete an image this process still has mapped.
// swapBinary leaves that leftover on disk. The remove here is
// best-effort for the same reason: the bytes are already at dst, and
// an error would skip the caller's state write. A later call tries
// earlier leftovers again, each under a new name, so a still-mapped
// leftover is never the destination of the vacate.
//
// A missing dst is one rename of bak onto that path, which is what
// the rollback did when there was nothing to replace.
//
// A hold another process has on a file is waited out only while dst is in
// place (backlog B898): dst and bak are probed, and dst moved aside, on one
// forward deadline (forwardWaits), set as the rollback begins, so the backup
// the rollbacks at a start have just run is waited for before anything moves,
// and a step that deadline has passed before is not tried
// (ErrDeadlinePassed); the place then has one attempt, adjacent to the vacate
// whenever the vacate lands, and a refused place is put back on the recovery
// schedule (recoveryWaits), the one wait with dst missing, which only shortens
// that time against giving up. Each failure is a *SwapError saying what it
// left; with dst missing it names the rename that puts it back.
func replaceRunningBinary(dst, bak string) error {
	return liveMoves().replace(dst, bak)
}

// replace is replaceRunningBinary through m.
func (m moves) replace(dst, bak string) error {
	fwd := m.deadlineOf(forwardWaits)
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		r := m.probeStep(bak, fwd)
		if r.err == nil {
			r = m.renameStep(StageInstall, bak, dst, forwardWaits, fwd)
		}
		if r.err != nil {
			e := failed(BinaryMissing, r, fmt.Sprintf("rollback rename %s -> %s: %v; the binary is not at its path: rename %s to %s",
				bak, dst, r.err, bak, dst))
			e.Recover = fmt.Sprintf("rename %s to %s", bak, dst)
			return e
		}
		return nil
	}

	removeStaleRollbackAsides(dst)
	aside := fmt.Sprintf("%s.rollback-%d", dst, time.Now().UnixNano())
	if r := m.probeStep(dst, fwd); r.err != nil {
		return failed(NothingMoved, r, fmt.Sprintf("rollback move %s aside: %v; nothing was moved", dst, r.err))
	}
	if r := m.probeStep(bak, fwd); r.err != nil {
		return failed(NothingMoved, r, fmt.Sprintf("rollback rename %s -> %s: %v; nothing was moved", bak, dst, r.err))
	}
	if r := m.renameStep(StageVacate, dst, aside, forwardWaits, fwd); r.err != nil {
		return failed(NothingMoved, r, fmt.Sprintf("rollback move %s aside: %v; nothing was moved", dst, r.err))
	}
	if r := m.once(StagePlace, bak, dst); r.err != nil {
		p := m.putBack(aside, dst)
		if p.err != nil {
			e := failed(BinaryMissing, p, fmt.Sprintf("rollback rename %s -> %s failed (%v); restore also failed (%v); "+
				"manual recovery needed: rename %s to %s, then start the bridge", bak, dst, r.err, p.err, aside, dst))
			e.Recover, e.errs = fmt.Sprintf("rename %s to %s", aside, dst), []error{r.err, p.err}
			return e
		}
		return failed(PutBack, r, fmt.Sprintf("rollback rename %s -> %s: %v (restored)", bak, dst, r.err))
	}
	if err := os.Remove(aside); err != nil && !os.IsNotExist(err) {
		logger.Warn("rollback left the previous binary beside the restored one; a mapped image cannot be deleted", "path", aside, "err", err)
	}
	return nil
}

// RestoreSeams are what a test's put-back of a backup moves files with
// (ReplaceRunningBinaryForTest): Rename makes one rename (os.Rename where
// nil), Probe is asked before each of the platform's own probes, its refusal
// the probe's, InUse says which other refusals are a hold besides the
// platform's own, Wait makes the wait before an attempt (time.Sleep where
// nil), or records it, and Now reads the clock the deadlines are kept on
// (time.Now where nil): a test whose Wait records a wait gives a clock that
// wait moves on by its length, so that its schedule's attempts do not depend
// on how long its steps take on the host.
type RestoreSeams struct {
	Rename func(oldpath, newpath string) error
	Probe  func(path string) error
	InUse  func(error) bool
	Wait   func(time.Duration)
	Now    func() time.Time
}

// ReplaceRunningBinaryForTest is Windows' put-back of a backup over its
// binary (replaceRunningBinary, without the check RollbackBinary makes that
// the backup is there), as a func of the binary's path and the backup's
// extension, as RollbackBinary takes them, on every platform, its files moved
// through s. A test of a caller on a host whose RollbackBinary is one rename
// passes it as the caller's restore (cmd/bridge's serveOpts.restoreBackup),
// so the caller meets what Windows' rollback answers. Production code MUST
// NOT call it.
func ReplaceRunningBinaryForTest(s RestoreSeams) func(dst, backupExt string) error {
	m := moves{rename: s.Rename, wait: s.Wait,
		inUse: func(err error) bool { return inUse(err) || (s.InUse != nil && s.InUse(err)) },
		probe: func(path string) error {
			if s.Probe != nil {
				if err := s.Probe(path); err != nil {
					return err
				}
			}
			return movable(path)
		}}
	if m.rename == nil {
		m.rename = os.Rename
	}
	if m.wait == nil {
		m.wait = time.Sleep
	}
	if m.now = s.Now; m.now == nil {
		m.now = time.Now
	}
	return func(dst, backupExt string) error { return m.replace(dst, dst+backupExt) }
}

// removeStaleRollbackAsides drops earlier vacated images that nothing
// maps anymore. A refusal leaves the file; the next vacate uses a new
// name. The listing is a directory read, not a glob: dst can contain
// glob metacharacters.
func removeStaleRollbackAsides(dst string) {
	dir := filepath.Dir(dst)
	prefix := filepath.Base(dst) + ".rollback-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}
