// Backlog B898 PR 1's tests come first in its history, over main's behaviour
// (this file, and the next commit, which removes it and lands the fix).

package admin

// A names-only shim of backlog B898's sentinels over main's console:
// classifyUpdateError knows none of them there.

import "errors"

var (
	ErrUpdateBinaryInUse       = errors.New("a file of the update is in use by another program")
	ErrUpdateBinaryMissing     = errors.New("the bridge's program file is not on disk")
	ErrUpdateMarkerNotRestored = errors.New("the install marker could not be put back")
)
