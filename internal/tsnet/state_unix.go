//go:build !windows

package tsnet

import (
	"fmt"
	"os"
	"syscall"
)

// assertSecureDir verifies the state directory is owned by the
// running process AND has 0700 permissions. POSIX-only — Windows
// ACLs don't map cleanly to mode bits, see state_windows.go for the
// alternative (which relies on the OS default).
//
// Why this matters: the state dir holds the bridge's tailnet machine
// identity. An attacker with read access to <stateDir>/tailscaled.state
// could authenticate as the bridge against the operator's tailnet —
// equivalent to stealing the bridge's bearer tokens AND its TLS
// pin. We explicitly fail closed rather than warning, because a
// quiet "your tailnet is compromised" log line in production is
// strictly worse than a loud "fix your perms before I'll start"
// error at boot.
func assertSecureDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("state path %s is not a directory", path)
	}

	// Permissions: any group or world bits trip us. 0700 is the
	// only acceptable value.
	perm := info.Mode().Perm()
	if perm&0o077 != 0 {
		return fmt.Errorf("state dir %s has perms %#o; chmod 0700 (group/world bits forbidden)", path, perm)
	}

	// Ownership: bridge must own its own state. A state dir owned
	// by some other user is either a deploy bug (root drops
	// privileges to the bridge user but didn't chown) or an
	// attacker prepositioning their own state to steal the next
	// auth.
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		uid := int(sys.Uid)
		expected := os.Getuid()
		if uid != expected {
			return fmt.Errorf("state dir %s owned by uid %d, not bridge uid %d", path, uid, expected)
		}
	}
	return nil
}
