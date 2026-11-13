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
// image (a sharing violation, reported as "Access is denied"). Both
// rollback callers run in this process, and the service stop is
// skipped when this process is the service, so a single rename of bak
// onto dst is the rename the kernel refuses. swapBinary already
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
// Each of those renames is made again on its own while it finds the
// file in use (rollbackRenames.try), never the rollback whole: a rename
// refused for that moved nothing, and each step starts where the one
// before it left the files. The backup is an executable of tens of
// megabytes that both rollbacks at a start have just run (its version,
// and the crash rollback's settings check), which Defender or the
// search indexer can still hold open: on a CI runner that refusal
// kept the release that crashed in place (backlog B872).
func replaceRunningBinary(dst, bak string) error {
	return rollbackRenames{rename: renameFunc, inUse: renameInUse, wait: rollbackWait}.replace(dst, bak)
}

// rollbackRenames is how the rollback renames: rename makes one rename,
// inUse says whether its failure is one another process's hold on the
// file ends, and wait sleeps before the next attempt.
type rollbackRenames struct {
	rename func(oldpath, newpath string) error
	inUse  func(error) bool
	wait   func(time.Duration)
}

// renameInUse and rollbackWait are rollbackRenames' inUse and wait in
// production: the platform's answer (inUseByAnotherProcess) and
// time.Sleep. Test seams, like renameFunc: production code MUST NOT
// mutate them.
var (
	renameInUse  = inUseByAnotherProcess
	rollbackWait = time.Sleep
)

// rollbackRenameBackoff is the wait before each attempt at one of the
// rollback's renames: the first at once, then 50 ms doubling to 1.6 s,
// seven attempts over 3.15 s. atomicwrite's schedule (five attempts over
// 750 ms) is for the scan-on-close of a small file it has just written;
// this file is an executable of tens of megabytes just run, which a
// scanner reads whole, and the Go toolchain's own renames wait 2 s on
// the same refusals (cmd/internal/robustio). One rollback makes three of
// these renames at most (the vacate, the backup's, the put-back), under
// 10 s, which fits beside the SCM's stop inside the install marker's
// lock's wait that every other taker gives it
// (TestTheRollbacksRenamesFitInsideTheMarkersLockWait).
var rollbackRenameBackoff = []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
	400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}

// rollbackRenameBudget is the longest one of the rollback's renames waits
// between its attempts: rollbackRenameBackoff's sum.
func rollbackRenameBudget() time.Duration {
	var total time.Duration
	for _, d := range rollbackRenameBackoff {
		total += d
	}
	return total
}

// replace is replaceRunningBinary's body, through r's renames.
func (r rollbackRenames) replace(dst, bak string) error {
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := r.try(bak, dst); err != nil {
			return fmt.Errorf("rollback rename %s -> %s: %w", bak, dst, err)
		}
		return nil
	}

	removeStaleRollbackAsides(dst)
	aside := fmt.Sprintf("%s.rollback-%d", dst, time.Now().UnixNano())
	if err := r.try(dst, aside); err != nil {
		return fmt.Errorf("rollback move %s aside: %w", dst, err)
	}
	if err := r.try(bak, dst); err != nil {
		if rerr := r.try(aside, dst); rerr != nil {
			return fmt.Errorf("rollback rename %s -> %s failed (%v); restore also failed (%v); manual recovery needed", bak, dst, err, rerr)
		}
		return fmt.Errorf("rollback rename %s -> %s: %w (restored)", bak, dst, err)
	}
	if err := os.Remove(aside); err != nil && !os.IsNotExist(err) {
		logger.Warn("rollback left the previous binary beside the restored one; a mapped image cannot be deleted", "path", aside, "err", err)
	}
	return nil
}

// try renames oldpath to newpath, and again after each of
// rollbackRenameBackoff's waits while the rename fails with the file in
// use. Any other failure is returned at once; one still in use after the
// last attempt is returned saying so.
func (r rollbackRenames) try(oldpath, newpath string) error {
	var err error
	for _, d := range rollbackRenameBackoff {
		if d > 0 {
			r.wait(d)
		}
		if err = r.rename(oldpath, newpath); err == nil || !r.inUse(err) {
			return err
		}
	}
	return fmt.Errorf("%w (still in use after %d attempts over %s)", err, len(rollbackRenameBackoff), rollbackRenameBudget())
}

// ReplaceRunningBinaryForTest is Windows' put-back of a backup over its
// binary (replaceRunningBinary, without the SCM's stop RollbackBinary
// makes first), as a func of the binary's path and the backup's
// extension, as RollbackBinary takes them, on every platform: its renames
// made by rename, a failure inUse answers yes for taken as the file in
// use (as the platform's own are), and its waits made by wait. A test of
// a caller on a host whose RollbackBinary is one rename passes it as the
// caller's restore (cmd/bridge's serveOpts.restoreBackup), so the caller
// meets what Windows' rollback answers. Production code MUST NOT call it.
func ReplaceRunningBinaryForTest(rename func(oldpath, newpath string) error, inUse func(error) bool,
	wait func(time.Duration)) func(dst, backupExt string) error {
	r := rollbackRenames{rename: rename, wait: wait,
		inUse: func(err error) bool { return inUseByAnotherProcess(err) || inUse(err) }}
	return func(dst, backupExt string) error { return r.replace(dst, dst+backupExt) }
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
