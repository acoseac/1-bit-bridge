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

// volumeID is the serial number of the volume holding dir, for SameVolume:
// read from a handle to the directory (FILE_FLAG_BACKUP_SEMANTICS opens a
// directory), as os.SameFile reads a file's identity.
func volumeID(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("utf16 %q: %w", dir, err)
	}
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, fmt.Errorf("open %q: %w", dir, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, fmt.Errorf("GetFileInformationByHandle %q: %w", dir, err)
	}
	return uint64(info.VolumeSerialNumber), nil
}

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
