package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
)

// ErrorWithoutPath describes err for a log line or an error of the bridge's
// own without the path an *fs.PathError in it names: such an error by its
// operation and the error under it ("read: input/output error"), and any
// other as it says itself. A file operation's error names the absolute path
// it was given, a library file's or a bridge directory's, and a log line
// names a library file library-relative and an operator's directory by its
// role (#1055); the caller says which file or directory beside it. One
// reading for every package that needs it: internal/manifest's walk and
// extractor lines (walkErrReason) and internal/transcode's disk probes.
//
// It never panics, whatever err holds: the scanner calls it in the walk's
// callback, which no recover covers, so a panic there ends the scan and the
// process with it. An *fs.PathError's own Error() dereferences its cause, so
// one without a cause (none that os builds) panics in anything that asks it
// for its text. Hence fmt, which answers "<nil>" for a nil error and
// recovers a panicking Error method.
func ErrorWithoutPath(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) && pe != nil {
		return pe.Op + ": " + fmt.Sprint(pe.Err)
	}
	return fmt.Sprint(err)
}
