//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package config

import "github.com/acoseac/1-bit-bridge/internal/fsutil"

// noLockToGive reports whether err, a try at the config file's lock, is
// flock's ENOLCK: no lock to give. An NFS mount whose lock manager is not
// running answers it, and so does a Linux kernel that cannot allocate a lock
// record, on any filesystem, while another writer holds the lock. So it never
// says the filesystem keeps no locks (fsutil.KeepsNoLocks, which counts it
// among those answers, is left as it is for its other readers): LockFile asks
// again, as it does while the lock is held. The reading is fsutil's
// (fsutil.NoLockToGive), which the store's lifetime lock shares (backlog B653
// PR 2a), so there is one copy of it.
func noLockToGive(err error) bool {
	return fsutil.NoLockToGive(err)
}
