//go:build !linux && !darwin && !windows

package doctor

import "context"

// pidListensOnPort is the second look at the recorded pid (procOwnerFunc) on
// a unix that is neither Linux nor macOS, which no release targets: there
// is no /proc socket table here, and what lsof's listing of a pid's own
// listeners shows (portowner_darwin.go) was measured on macOS alone.
//
// (false, nil) is "asked and got no match", lsof's own answer for a port it
// cannot attribute. The sighting says nothing looked, which leaves the
// recorded bridge possible: without lsof the caller puts the missing lsof in
// front of it, and after lsof's miss it adds nothing to lsof's account
// (procSecondOpinion).
func pidListensOnPort(context.Context, int, int) (bool, ownerSighting, error) {
	return false, ownerSighting{saw: nothingElseMatches}, nil
}
