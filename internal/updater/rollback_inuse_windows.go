//go:build windows

package updater

import (
	"errors"

	"golang.org/x/sys/windows"
)

// inUseByAnotherProcess reports whether a rename failed because another
// process has the file open without sharing its deletion
// (ERROR_SHARING_VIOLATION: Defender or the search indexer reading an
// executable just written or run) or holds a lock on a range of it
// (ERROR_LOCK_VIOLATION): refusals that end once that process lets the
// file go. Not ERROR_ACCESS_DENIED, which a rename onto a mapped image
// answers (backlog B202) and a permission does, neither of which a wait
// changes.
func inUseByAnotherProcess(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
