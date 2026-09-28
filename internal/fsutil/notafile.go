package fsutil

import (
	"errors"
	"io/fs"
	"os"
)

// notAFileKinds are the kinds of entry that do not open as a file, each with
// the name NotAFile gives it, checked in order: a character device carries
// ModeDevice too.
var notAFileKinds = []struct {
	bit  fs.FileMode
	name string
}{
	{fs.ModeDir, "directory"},
	{fs.ModeNamedPipe, "named pipe"},
	{fs.ModeSocket, "socket"},
	{fs.ModeCharDevice, "character device"},
	{fs.ModeDevice, "device"},
}

// NotAFile names the kind of an entry whose stat reports m when it does not
// open as a file, and answers "" when it does: for a regular file, and for a
// Windows reparse point that names nothing (ModeIrregular), such as a cloud
// placeholder, which opens, and hydrates, like a file. That is why the test
// is a list of kinds refused and not "is a regular file": on Windows a
// OneDrive library with files on demand is ModeIrregular throughout.
//
// It is the one list for both sides of the library: the scanner indexes no
// entry it names (internal/manifest's walkedFileInfo), and no route that
// serves a file's bytes serves one (OpenAsFile). A second copy is how the
// two would come to disagree about what a track can be.
func NotAFile(m fs.FileMode) string {
	for _, k := range notAFileKinds {
		if m&k.bit != 0 {
			return k.name
		}
	}
	return ""
}

// NotAFileError is OpenAsFile's refusal of an entry that does not open as a
// file (NotAFile), carried inside the *fs.PathError that names the path.
type NotAFileError struct {
	// Kind is NotAFile's name for the entry: "named pipe", "directory", …
	Kind string
}

// Error names the kind refused, never the path: the *fs.PathError around it
// carries that, and a caller logging a library file strips the absolute
// path from it (#1055).
func (e *NotAFileError) Error() string { return e.Kind + " is not a file" }

// NotAFileKind returns the kind OpenAsFile refused when err is, or wraps, a
// *NotAFileError, and "" for any other error.
func NotAFileKind(err error) string {
	var nf *NotAFileError
	if errors.As(err, &nf) {
		return nf.Kind
	}
	return ""
}

// OpenAsFile opens name for reading its bytes, the way every route that
// serves a file opens it, and returns the opened file's own stat, which
// describes the bytes it will read. What does not open as a file (NotAFile)
// is refused with a *NotAFileError, and a named pipe is refused without
// waiting for a writer.
//
// A plain os.Open of a named pipe waits for a writer, and nothing can cancel
// the wait: open(2) is a blocking system call, and Go has no way to
// interrupt one. Until 2026-09-28 /v1/download, /v1/read, the web player's
// audio route and the DLNA file route opened library paths with os.Open, so
// a FIFO named like a track held its request forever: the client gave up at
// its own timeout while the handler, and the updater session it had begun,
// stayed until something wrote to the pipe, and a pinned session keeps
// auto-install deferring on every poll. The scanner had stopped indexing such
// entries that day (#1070), but a route serves any path a client names, and
// a manifest row outlives the file it was minted for until the scan that
// reaps it.
//
// On unix the open is nonblocking (openNoWait), which lets a named pipe's
// open return at once, and the refusal reads the stat of the file that
// opened, never a stat taken before: a path replaced between a caller's stat
// and this open is judged as what it is now. The flag is cleared again once
// the file is known to be one (setBlocking), so its reads are the reads a
// plain open gives. Elsewhere it is os.Open followed by the same refusal.
//
// A caller that already holds a stat of the path (a resolver's) checks
// NotAFile against it before calling this, so a device is refused without
// being opened at all; this refusal is for a path that changed in between,
// and for a caller with no stat of its own.
//
// Open errors come back exactly as os.OpenFile returns them, so
// os.IsNotExist and errors.Is(err, fs.ErrNotExist) answer as they did. A
// socket is one of them: no open reaches one, and the kernel refuses it
// itself (EOPNOTSUPP on macOS, ENXIO on Linux), before there is a file to
// stat.
func OpenAsFile(name string) (*os.File, fs.FileInfo, error) {
	f, nonblocking, err := openNoWait(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if kind := NotAFile(info.Mode()); kind != "" {
		_ = f.Close()
		return nil, nil, &fs.PathError{Op: "open", Path: name, Err: &NotAFileError{Kind: kind}}
	}
	if nonblocking {
		if err := setBlocking(f); err != nil {
			_ = f.Close()
			return nil, nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
	}
	return f, info, nil
}
