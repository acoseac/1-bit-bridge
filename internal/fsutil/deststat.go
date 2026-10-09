package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// DestPresence is what a stat of a path a rename would replace found.
type DestPresence int

const (
	// DestAbsent is a path that is not there.
	DestAbsent DestPresence = iota
	// DestPresent is a path that exists. A rename onto it would replace that file.
	DestPresent
	// DestUnreadable is a stat that failed for any reason other than the path
	// not existing. That is not "nothing there".
	DestUnreadable
)

const destinationUnreadable = "could not check whether a file is already at this path"

// StatDestination stats path through seam, or with os.Stat when seam is nil.
// The reason for DestUnreadable names the error and not the path: a PathError
// would echo the absolute library root, and the caller already records the
// library-relative path.
func StatDestination(seam func(string) (os.FileInfo, error), path string) (DestPresence, string) {
	var err error
	if seam != nil {
		_, err = seam(path)
	} else {
		_, err = os.Stat(path)
	}
	switch {
	case err == nil:
		return DestPresent, ""
	case errors.Is(err, fs.ErrNotExist):
		return DestAbsent, ""
	default:
		return DestUnreadable, unreadableDestination(err)
	}
}

// StatIOFault is a stat failure whose text names no real path. Tests inject
// it so a destination check can fail without a fault on the volume.
func StatIOFault(string) (os.FileInfo, error) {
	return nil, &os.PathError{Op: "stat", Path: "hidden", Err: errors.New("input/output error")}
}

func unreadableDestination(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Err != nil {
		err = pe.Err
	}
	return destinationUnreadable + ": " + err.Error()
}

// RejectUnreadableReason reports a destination-stat reason that is missing
// the unreadable text or that still names abs.
func RejectUnreadableReason(reason, abs string) error {
	if !strings.Contains(reason, destinationUnreadable) {
		return fmt.Errorf("reason = %q", reason)
	}
	if strings.Contains(reason, abs) {
		return fmt.Errorf("reason names the absolute path: %q", reason)
	}
	return nil
}
