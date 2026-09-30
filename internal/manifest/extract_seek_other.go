//go:build !windows

package manifest

import (
	"errors"
	"syscall"
)

// seekOffsetRefused reports whether err is a seek's refusal of the offset it
// was asked for, which lseek answers with EINVAL (a resulting offset before
// the start of the file) before it does anything else: an answer about the
// offset, which a parser computes from the file's bytes, never a failure to
// reach the file (faultNotingSource).
func seekOffsetRefused(err error) bool {
	return errors.Is(err, syscall.EINVAL)
}
