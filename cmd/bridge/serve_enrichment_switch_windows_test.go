//go:build windows

package main

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// TestTheStandInRetryKeepsWindowsOwnRetry: with the stand-in hold's retry in
// place, as TestTheHarvestHelperReadsPastAStandInHold has it, a read refused
// once with Windows' sharing violation, then once with access denied, is
// still asked again and returns the covers, and so is one refused a second
// time with the sharing violation, as a scanner's hold would (backlog B898).
func TestTheStandInRetryKeepsWindowsOwnRetry(t *testing.T) {
	retryStandIn(t)
	platform := func(err error) bool {
		return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	readsPastOneRefusal(t, platform, nil, windows.ERROR_SHARING_VIOLATION, windows.ERROR_ACCESS_DENIED)
	readsPastOneRefusal(t, platform, windows.ERROR_SHARING_VIOLATION, windows.ERROR_SHARING_VIOLATION, windows.ERROR_ACCESS_DENIED)
}
