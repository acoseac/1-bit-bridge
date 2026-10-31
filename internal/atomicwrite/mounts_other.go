//go:build !linux && !darwin && !windows

package atomicwrite

// mountIDsExpected is true here though the platform gives the strict writer
// no mount identity it reads: these systems have a directory barrier, and
// the writer refuses (ErrMountsUnknown) rather than take two mounts for one.
// The player, its one caller, runs on Linux alone.
const mountIDsExpected = true

// platformInspect takes every root: the strict writer refuses on this
// platform before it asks, for want of a mount identity (ErrMountsUnknown).
func platformInspect(string, uint64) Medium { return Medium{} }

// platformCopyUps is none: the strict writer refuses on this platform before
// it asks.
func platformCopyUps(string, uint64) ([]string, error) { return nil, nil }

// platformMountID is no ID: the strict writer reads none on this platform.
func platformMountID(string) (uint64, bool, error) { return 0, false, nil }
