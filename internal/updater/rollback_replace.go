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
func replaceRunningBinary(dst, bak string) error {
	if _, err := os.Stat(dst); errors.Is(err, os.ErrNotExist) {
		if err := renameFunc(bak, dst); err != nil {
			return fmt.Errorf("rollback rename %s -> %s: %w", bak, dst, err)
		}
		return nil
	}

	removeStaleRollbackAsides(dst)
	aside := fmt.Sprintf("%s.rollback-%d", dst, time.Now().UnixNano())
	if err := renameFunc(dst, aside); err != nil {
		return fmt.Errorf("rollback move %s aside: %w", dst, err)
	}
	if err := renameFunc(bak, dst); err != nil {
		if rerr := renameFunc(aside, dst); rerr != nil {
			return fmt.Errorf("rollback rename %s -> %s failed (%v); restore also failed (%v); manual recovery needed", bak, dst, err, rerr)
		}
		return fmt.Errorf("rollback rename %s -> %s: %w (restored)", bak, dst, err)
	}
	if err := os.Remove(aside); err != nil && !os.IsNotExist(err) {
		logger.Warn("rollback left the previous binary beside the restored one; a mapped image cannot be deleted", "path", aside, "err", err)
	}
	return nil
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
