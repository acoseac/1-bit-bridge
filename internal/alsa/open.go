package alsa

import (
	"errors"
	"os"
)

// The reasons an opened path is refused.
var (
	errNotAFile      = errors.New("not a regular file")
	errNotADirectory = errors.New("not a directory")
)

// openRegular opens a file to read it through open, which is handed the
// flags, and refuses it unless the opened descriptor is a regular file.
//
// A FIFO opened for reading blocks until a writer comes, and a tree the
// readers are pointed at (--proc-root, --sysfs-root, a fake tree) may hold
// one where a file should be, or have one swapped in after a check. So the
// open asks not to block (openNonblock), and what decides is the type of the
// descriptor that opened, never a look at the path before it.
func openRegular(open func(flag int) (*os.File, error)) (*os.File, error) {
	f, err := open(os.O_RDONLY | openNonblock)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = errNotAFile
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// openDirectory opens a directory to list it through open, which is handed
// the flags: directory-only (openDirectoryOnly), so a FIFO there fails at
// once, and not blocking all the same. It refuses what the opened descriptor
// says is not a directory.
func openDirectory(open func(flag int) (*os.File, error)) (*os.File, error) {
	f, err := open(os.O_RDONLY | openDirectoryOnly | openNonblock)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.IsDir() {
		err = errNotADirectory
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// pathOpener opens path on the host, following a link as os.Open does.
func pathOpener(path string) func(flag int) (*os.File, error) {
	return func(flag int) (*os.File, error) { return os.OpenFile(path, flag, 0) }
}
