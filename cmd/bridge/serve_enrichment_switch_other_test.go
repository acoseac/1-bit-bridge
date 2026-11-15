//go:build !windows

package main

import (
	"errors"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// errPlatformForTest stands in for an error the platform's own predicate
// asks again on, which on Linux and macOS no error is.
var errPlatformForTest = errors.New("an error the platform asks again on")

// TestTheStandInRetryKeepsWhatWasAskedAgainOnBefore: the stand-in hold's
// retry adds errBusyForTest to what atomicwrite asked again on before it and
// replaces none of it, so the stand-in's test still waits out a refusal of the
// platform's own (on Windows a scanner's hold of the file, backlog B898). A
// predicate standing in for the platform's is installed first: synthetic
// coverage of the composition off Windows, once with a second refusal of the
// platform's after the first. It does not build on Windows, where a real
// sharing violation on a later call is no error its predicate takes; Windows'
// own codes are TestTheStandInRetryKeepsWindowsOwnRetry's.
func TestTheStandInRetryKeepsWhatWasAskedAgainOnBefore(t *testing.T) {
	platform := func(err error) bool { return errors.Is(err, errPlatformForTest) }
	t.Cleanup(atomicwrite.SetReadRetryableForTest(platform))
	retryStandIn(t)
	readsPastOneRefusal(t, platform, nil, errPlatformForTest, errBusyForTest)
	readsPastOneRefusal(t, platform, errPlatformForTest, errPlatformForTest, errBusyForTest)
}
