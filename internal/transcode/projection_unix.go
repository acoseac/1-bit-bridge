//go:build !windows

// POSIX implementation of AvailableDiskSpace. Uses syscall.Statfs to
// read the free-byte count on the volume containing `dir`. Identical
// on Linux + macOS + every BSD; the structure layout differs between
// platforms but the (Bavail, Bsize) fields are universal.

package transcode

import (
	"fmt"
	"os"
	"syscall"
)

// volumeID is the device holding dir, for SameVolume. Dev is an int32 on
// darwin and a uint64 on linux; widened, equal devices stay equal.
func volumeID(dir string) (uint64, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %q: no device", dir)
	}
	return uint64(st.Dev), nil
}

// AvailableDiskSpace returns the number of bytes available to a
// non-privileged caller on the volume that holds `dir`. The probe is
// non-recursive — Statfs reads the volume's superblock-equivalent
// stats, not a per-directory quota.
//
// Bavail (rather than Bfree) is the right choice: Bfree includes
// reserved space the kernel won't actually let a non-root caller
// allocate. On a typical Linux mount the gap is ~5% of the volume —
// using Bfree would consistently over-report and let DiskHasHeadroom
// approve batches that hit -ENOSPC mid-write.
func AvailableDiskSpace(dir string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", dir, err)
	}
	// Bavail / Bsize types vary across platforms (uint64 vs int64);
	// the multiplication promotes to int64 unambiguously after the
	// explicit cast.
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
