//go:build !unix

package atomicwrite

import "io/fs"

// devicesKnown is false where a FileInfo carries no device: Windows, where
// the strict writer refuses before it looks at a path, having no directory
// barrier (ErrNoDirectoryBarrier), so only a test that sets a barrier gets
// as far as plain, which then compares no devices.
const devicesKnown = false

// device is no answer here.
func device(fs.FileInfo) (uint64, bool) { return 0, false }
