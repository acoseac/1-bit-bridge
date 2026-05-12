//go:build windows

// Windows implementation of AvailableDiskSpace. Uses
// GetDiskFreeSpaceExW from golang.org/x/sys/windows, which returns
// the number of bytes available to the caller on the volume that
// holds `dir`. The "Ex" form (vs the older non-Ex variant) supports
// volumes > 2 GB and per-user quotas.

package transcode

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// AvailableDiskSpace returns the bytes-available count on the volume
// containing `dir`. Per the Windows API contract, the first return
// from GetDiskFreeSpaceEx is the number of bytes free to the calling
// caller — already quota-aware on a per-user-quota volume, matching
// what the POSIX implementation reports via statfs's Bavail field.
func AvailableDiskSpace(dir string) (int64, error) {
	utf16Dir, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("utf16 %q: %w", dir, err)
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(
		utf16Dir,
		&freeBytesAvailable,
		&totalBytes,
		&totalFreeBytes,
	); err != nil {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx %q: %w", dir, err)
	}
	if freeBytesAvailable > uint64(1<<63-1) {
		// Defensive clamp on a volume reporting > 8 EB to a 32-bit
		// build; not realistic on real hardware but the conversion
		// to int64 would otherwise wrap.
		return 1<<63 - 1, nil
	}
	return int64(freeBytesAvailable), nil
}
