//go:build !windows

package tsnet

import (
	"fmt"
	"os"
	"syscall"
)

// chmodStateDir tightens the directory's permission bits to `mode`.
// Used by Start to defend against the case where the state dir
// pre-existed with looser perms (Qodo bug #1) — os.MkdirAll doesn't
// modify existing dirs, so MkdirAll-then-assertSecureDir would
// otherwise fail closed with no operator-visible attempt to fix.
func chmodStateDir(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}

// assertSecureDir verifies the state directory is owned by the
// running process AND has exactly 0700 permissions. POSIX-only —
// Windows ACLs don't map cleanly to mode bits, see state_windows.go
// for the alternative (which relies on the OS default).
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

	// Permissions: require exactly 0700. The earlier check just
	// rejected group/world bits, so 0500 (no owner write) would
	// pass — but tsnet needs to write state into the dir, so 0500
	// is misconfigured AND a security guard that accepts it is
	// strictly weaker than necessary. Qodo bug #2 on PR #138.
	perm := info.Mode().Perm()
	if perm != 0o700 {
		return fmt.Errorf("state dir %s has perms %#o; want exactly 0700", path, perm)
	}

	// Ownership: bridge must own its own state. A state dir owned
	// by some other user is either a deploy bug (root drops
	// privileges to the bridge user but didn't chown) or an
	// attacker prepositioning their own state to steal the next
	// auth.
	//
	// Fail closed if we can't read the ownership metadata (e.g.
	// non-Linux POSIX where the Sys() shape may differ) — a guard
	// that silently passes on a missing check is worse than no
	// check at all. CodeRabbit Major on PR #138.
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("state dir %s: cannot read ownership metadata (unexpected stat shape)", path)
	}
	uid := int(sys.Uid)
	expected := os.Getuid()
	if uid != expected {
		return fmt.Errorf("state dir %s owned by uid %d, not bridge uid %d", path, uid, expected)
	}
	return nil
}
