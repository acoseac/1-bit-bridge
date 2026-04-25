//go:build windows

package updater

import (
	"fmt"
	"io"
	"os"
)

// Phase B does not yet ship Windows self-install. The rename-trick
// (rename current bridge.exe → bridge.exe.bak first, then write new
// bytes into bridge.exe) works on Windows but interacts subtly with
// SCM-managed services (the file may be locked depending on how the
// service was installed) and with administrative-permission
// requirements for installs under Program Files. We'd rather defer
// that complexity to a follow-up than ship a half-tested swap path
// that bricks operator installs.
//
// On Windows the install path returns ErrInstallNotSupported
// straight from the Updater; the admin UI hides the Install button.
// Operators on Windows still upgrade by stopping the service,
// replacing bridge.exe manually, and starting it back up — exactly
// the pre-Phase-B workflow.

func swapBinary(dst, newBinary, backupExt string) error {
	return ErrInstallNotSupported
}

func RollbackBinary(dst, backupExt string) error {
	return ErrInstallNotSupported
}

func RemoveBackup(dst, backupExt string) error {
	// Even on Windows, removing a stale .bak is harmless and worth
	// supporting so a future Phase-B-Windows can leave the cleanup
	// invariants the same.
	bak := dst + backupExt
	err := os.Remove(bak)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// io / fmt may not be used by all build paths but are kept here so a
// future Windows swap implementation can drop them in without
// rearranging the import set.
var _ = io.EOF
var _ = fmt.Errorf
