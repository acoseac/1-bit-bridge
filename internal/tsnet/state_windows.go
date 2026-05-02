//go:build windows

package tsnet

import (
	"fmt"
	"os"
)

// assertSecureDir is a no-op-ish sanity check on Windows.
//
// POSIX 0700 / uid checks don't translate cleanly to Windows ACLs:
//
//   - Permission bits returned by os.Stat are a synthesised view that
//     doesn't reflect the actual ACL — checking for `perm & 0077 != 0`
//     would either miss real lockdowns or trip on benign setups.
//   - There's no portable "running uid" concept; SIDs aren't
//     comparable to ints.
//
// Windows deploys are expected to rely on the default %APPDATA% /
// %ProgramData% ACL inherited by the directory. An operator who
// deliberately puts the state dir on a shared volume (rare but
// possible — e.g. cluster-shared file system) is responsible for
// the ACL.
//
// This function still verifies the path is actually a directory —
// catches the "state dir is a file" config typo regardless of OS.
// Documented in the operator docs so a Windows deploy doesn't hit
// the more aggressive POSIX check by accident.
func assertSecureDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("state path %s is not a directory", path)
	}
	return nil
}
