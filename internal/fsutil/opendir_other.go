//go:build !unix

package fsutil

import (
	"io/fs"
	"os"
	"syscall"
)

// openDir is os.Open on a system with no O_DIRECTORY to ask for, followed by
// the refusal the flag would have made: a file that is not a directory is
// closed and answered with ENOTDIR. On Windows, the one such system the
// bridge ships for, the open of a named pipe does not wait for its server
// (openNoWait's docblock), so the open itself cannot hold the caller.
func openDir(name string) (*os.File, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.IsDir() {
		_ = f.Close()
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.ENOTDIR}
	}
	return f, nil
}
