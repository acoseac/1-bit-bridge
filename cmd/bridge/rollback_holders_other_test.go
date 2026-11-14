//go:build !windows

package main

import "errors"

// windowsErrorCode is the Windows error code a refused rename carries, and
// whether it says a file is in use: none off Windows, where the rollback's
// put-back (Windows' RollbackBinary) does not run.
func windowsErrorCode(error) (uint32, bool) { return 0, false }

// candidateUsers asks the Restart Manager, which is Windows' alone.
func candidateUsers(string) ([]candidateUser, error) {
	return nil, errors.New("the Restart Manager is Windows'")
}
