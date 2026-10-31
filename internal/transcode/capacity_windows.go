//go:build windows

package transcode

import (
	"golang.org/x/sys/windows"
)

// TotalDiskSpace returns the capacity of the volume that holds dir. A
// failure names the call and the errno, never dir (probeError).
//
// GetDiskFreeSpaceEx's second out-parameter is the total number of bytes on
// the volume available to the caller, which is the counterpart of the
// bytes-available figure AvailableDiskSpace reports — so the two agree about
// what "the volume" means on a per-user-quota mount.
func TotalDiskSpace(dir string) (int64, error) {
	utf16Dir, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, &probeError{op: "utf16", err: err}
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(
		utf16Dir, &freeBytesAvailable, &totalBytes, &totalFreeBytes,
	); err != nil {
		return 0, &probeError{op: "GetDiskFreeSpaceEx", err: err}
	}
	if totalBytes > uint64(1<<63-1) {
		return 1<<63 - 1, nil
	}
	return int64(totalBytes), nil
}
