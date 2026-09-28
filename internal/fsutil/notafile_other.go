//go:build !unix

package fsutil

import "os"

// openNoWait is os.Open on a system with no O_NONBLOCK to ask for. On
// Windows, the one such system the bridge ships for, opening a named pipe
// does not wait for its server (CreateFile fails with ERROR_PIPE_BUSY
// instead), so OpenAsFile's refusal of the opened file's stat is the whole
// check. The file is never left nonblocking.
func openNoWait(name string) (*os.File, bool, error) {
	f, err := os.Open(name)
	return f, false, err
}

// setBlocking is never called here, since openNoWait leaves nothing to
// clear; it exists for OpenAsFile to compile.
func setBlocking(*os.File) error { return nil }
