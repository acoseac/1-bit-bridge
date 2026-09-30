package fsutil

import "os"

// OpenDir opens the directory name to read its entries (File.Readdir,
// File.ReadDir), and refuses at once whatever else is there: a path that is
// not a directory, or no longer one, answers an *fs.PathError holding
// syscall.ENOTDIR, as it does when a component of the path is not a
// directory.
//
// A plain os.Open of a named pipe waits for a writer, and nothing can cancel
// the wait (OpenAsFile's docblock). /v1/list opened the directory it lists,
// which the resolver's stat had just called one, with os.Open, so a
// directory replaced by a named pipe between that stat and the open held the
// request until something wrote to the pipe (until 2026-09-29).
//
// On unix the open asks for O_DIRECTORY, which the kernel checks before it
// opens anything, so a named pipe, a device, a socket or a file is refused
// with ENOTDIR without waiting (measured on macOS and Linux: in microseconds,
// where os.Open of the pipe was still waiting two seconds later). Elsewhere,
// which for the bridge is Windows, whose open of a named pipe does not wait
// for its server (CreateFile fails with ERROR_PIPE_BUSY instead), it is
// os.Open, and a file that turns out not to be a directory is closed and
// refused with the same error.
//
// os.ReadDir and filepath.WalkDir need none of this: they open a directory
// with O_DIRECTORY already (the os package's openDir, in the go1.26.6 this
// module builds with; measured, os.ReadDir of a named pipe answers ENOTDIR at
// once). It is for a caller that opens the directory itself, as the listing
// does to read Lstat-shaped entries with Readdir.
func OpenDir(name string) (*os.File, error) {
	return openDir(name)
}
