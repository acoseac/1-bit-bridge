package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// EntryInTheWay is an entry of a path that is there and is no directory, so
// that no directory can be made at it or below it: what bridge serve's
// refusal of a data directory it cannot prepare names, and bridge doctor's
// data-dir and player-storage lines with it (B606, B659).
type EntryInTheWay struct {
	// Entry is the entry, spelled as the path spells it.
	Entry string
	// Link says the entry is a symbolic link.
	Link bool
	// Target is where a link's links lead (filepath.EvalSymlinks), "" for a
	// link to nothing, for a link whose links could not be resolved, and for
	// an entry that is no link.
	Target string
	// Kind is what stands there, with its article (EntryKind: "a regular
	// file", "a named pipe", …): at the end of a link's links, and otherwise
	// the entry's own; "" for a link to nothing.
	Kind string
}

// String says what stands there: "E is a regular file, not a directory", "E
// leads to T, a regular file, not a directory", "E is a symbolic link to a
// regular file, not a directory" for a link whose links could not be
// resolved, and "E is a symbolic link to nothing".
func (e *EntryInTheWay) String() string {
	switch {
	case e.Link && e.Kind == "":
		return e.Entry + " is a symbolic link to nothing"
	case e.Link && e.Target == "":
		return fmt.Sprintf("%s is a symbolic link to %s, not a directory", e.Entry, e.Kind)
	case e.Link:
		return fmt.Sprintf("%s leads to %s, %s, not a directory", e.Entry, e.Target, e.Kind)
	}
	return fmt.Sprintf("%s is %s, not a directory", e.Entry, e.Kind)
}

// NotADirectory says what stands at entry when it is there and is no
// directory: a regular file or another kind of entry, a symbolic link that
// leads to one, or a symbolic link to nothing. Whether it is a directory is
// os.Stat's answer, as for os.MkdirAll and bridge serve's check after it, so a
// directory, a link that leads to one, and a Windows mount point that leads to
// one, which os.Lstat calls an irregular file (Go 1.23's reading of a name
// surrogate), are nil, nil; a mount point that leads nowhere is in the way, as
// os.Lstat names it. An entry that cannot be read is nil and the error:
// os.Lstat's (fs.ErrNotExist for one that is not there), or for an entry
// there, os.Stat's other than fs.ErrNotExist (a link cycle, a link through a
// regular file on unix, a permission error).
func NotADirectory(entry string) (*EntryInTheWay, error) {
	linfo, err := os.Lstat(entry)
	if err != nil {
		return nil, err
	}
	link := linfo.Mode()&fs.ModeSymlink != 0
	if !link && linfo.IsDir() {
		return nil, nil
	}
	info, err := os.Stat(entry)
	switch {
	case link && errors.Is(err, fs.ErrNotExist):
		return &EntryInTheWay{Entry: entry, Link: true}, nil
	case errors.Is(err, fs.ErrNotExist):
		return &EntryInTheWay{Entry: entry, Kind: EntryKind(linfo.Mode())}, nil
	case err != nil:
		return nil, err
	case info.IsDir():
		return nil, nil
	case !link:
		return &EntryInTheWay{Entry: entry, Kind: EntryKind(info.Mode())}, nil
	}
	in := &EntryInTheWay{Entry: entry, Link: true, Kind: EntryKind(info.Mode())}
	if resolved, err := filepath.EvalSymlinks(entry); err == nil {
		in.Target = resolved
	}
	return in, nil
}

// BlockingEntry walks up path, from path itself, past every entry os.Lstat
// finds not there or below an entry that is no directory (fs.ErrNotExist,
// ENOTDIR), to the first entry of path that is there (nearest), and says what
// stands there when it is no directory (NotADirectory), which is then the
// entry in the way of making path a directory (os.MkdirAll stops there: ENOTDIR
// at a regular file or a link to one, EEXIST at a link to nothing). It reads
// and never writes. A directory there, or a link to one, is no blocker
// (nearest set, blocker nil): path is that directory, or is made below it.
// err is an os.Lstat on the way that failed for another reason (a permission
// error, a link cycle), the reading of the entry there (NotADirectory's), or
// the last os.Lstat's where no entry of path is there at all; nearest is ""
// with it.
func BlockingEntry(path string) (nearest string, blocker *EntryInTheWay, err error) {
	for p := path; ; {
		_, lerr := os.Lstat(p)
		switch {
		case lerr == nil:
			in, nerr := NotADirectory(p)
			if nerr != nil {
				return "", nil, nerr
			}
			return p, in, nil
		case !errors.Is(lerr, fs.ErrNotExist) && !errors.Is(lerr, syscall.ENOTDIR):
			return "", nil, lerr
		}
		up := filepath.Dir(p)
		if up == p {
			return "", nil, lerr
		}
		p = up
	}
}
