//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package config

// noLockToGive is noLockToGive where the config's lock is not flock: no
// answer there means no lock to give.
func noLockToGive(error) bool { return false }
