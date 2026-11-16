//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package config

import "github.com/acoseac/1-bit-bridge/internal/fsutil"

// noLockToGive is noLockToGive where the config's lock is not flock: no
// answer there means no lock to give (fsutil.NoLockToGive answers false).
func noLockToGive(err error) bool { return fsutil.NoLockToGive(err) }
