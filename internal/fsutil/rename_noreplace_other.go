//go:build !(linux || darwin || windows)

package fsutil

import (
	"errors"
	"runtime"
)

// errNoReplaceCall is platformRenameNoReplace's answer on a platform where
// this package makes no rename that never replaces.
var errNoReplaceCall = errors.New("no such call on " + runtime.GOOS)

// platformRenameNoReplace renames nothing here: this package makes no
// rename that never replaces on this platform, and no other rename in its
// place.
func platformRenameNoReplace(string, string) error { return errNoReplaceCall }

// noReplaceUnsupported reads errNoReplaceCall, and errors.ErrUnsupported, as
// no such rename here.
func noReplaceUnsupported(err error) bool {
	return errors.Is(err, errNoReplaceCall) || errors.Is(err, errors.ErrUnsupported)
}

// noReplaceTransient is false here: there is no call to try again.
func noReplaceTransient(error) bool { return false }
