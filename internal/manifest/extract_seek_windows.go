//go:build windows

package manifest

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// seekOffsetRefused reports whether err is a seek's refusal of the offset it
// was asked for: on Windows SetFilePointerEx answers a resulting offset
// before the start of the file with ERROR_NEGATIVE_SEEK, where lseek says
// EINVAL (extract_seek_other.go). An answer about the offset, which a parser
// computes from the file's bytes, never a failure to reach the file
// (faultNotingSource).
func seekOffsetRefused(err error) bool {
	return errors.Is(err, windows.ERROR_NEGATIVE_SEEK) || errors.Is(err, syscall.EINVAL)
}
