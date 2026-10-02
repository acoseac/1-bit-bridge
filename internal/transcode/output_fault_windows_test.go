//go:build windows

package transcode

import (
	"io/fs"
	"syscall"

	"golang.org/x/sys/windows"
)

// outputFaultErrnosWant is the Windows table, written out: what each cause
// says about the output side's volume.
func outputFaultErrnosWant() map[syscall.Errno]outputFaultKind {
	return map[syscall.Errno]outputFaultKind{
		windows.ERROR_WRITE_PROTECT:       outputReadOnly,
		windows.ERROR_DISK_FULL:           outputFull,
		windows.ERROR_HANDLE_DISK_FULL:    outputFull,
		windows.ERROR_DISK_QUOTA_EXCEEDED: outputQuota,
		windows.ERROR_IO_DEVICE:           outputGone,
		windows.ERROR_NOT_READY:           outputGone,
		windows.ERROR_DEV_NOT_EXIST:       outputGone,
		windows.ERROR_NETNAME_DELETED:     outputGone,
	}
}

// permissionErrnos are the causes fs.ErrPermission names on Windows.
func permissionErrnos() []syscall.Errno {
	return []syscall.Errno{windows.ERROR_ACCESS_DENIED, syscall.EACCES, syscall.EPERM}
}

// notOutputFaultCauses are failures at an output step that keep their
// strike: what the source's name produces, and entries that are, or are not,
// already there.
func notOutputFaultCauses() []error {
	var out []error
	for _, errno := range []syscall.Errno{
		windows.ERROR_FILENAME_EXCED_RANGE, windows.ERROR_INVALID_NAME,
		windows.ERROR_FILE_EXISTS, windows.ERROR_ALREADY_EXISTS,
		windows.ERROR_PATH_NOT_FOUND, windows.ERROR_FILE_NOT_FOUND,
	} {
		out = append(out, &fs.PathError{Op: "open", Path: `C:\variants\x`, Err: errno})
	}
	return out
}

func readOnlyVolumeErrno() syscall.Errno { return windows.ERROR_WRITE_PROTECT }
func fullVolumeErrno() syscall.Errno     { return windows.ERROR_DISK_FULL }
