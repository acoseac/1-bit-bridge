//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package config

import (
	"errors"

	"golang.org/x/sys/unix"
)

// noLockToGive reports whether err, a try at the config file's lock, is
// flock's ENOLCK: no lock to give. An NFS mount whose lock manager is not
// running answers it, and so does a Linux kernel that cannot allocate a lock
// record, on any filesystem, while another writer holds the lock. So it never
// says the filesystem keeps no locks (fsutil.KeepsNoLocks, which counts it
// among those answers, is left as it is for its other readers): LockFile asks
// again, as it does while the lock is held.
func noLockToGive(err error) bool {
	return errors.Is(err, unix.ENOLCK)
}
