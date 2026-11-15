//go:build windows

package main

import (
	"errors"
	"os"
	"sync"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// TestTheStandInRetryKeepsWindowsOwnRetry: with the stand-in hold's retry in
// place, as TestTheHarvestHelperReadsPastAStandInHold has it, a read refused
// once with Windows' sharing violation, then once with access denied, is
// still asked again and returns the covers (backlog B898).
func TestTheStandInRetryKeepsWindowsOwnRetry(t *testing.T) {
	retryStandIn(t)
	readsPastOneRefusal(t, windows.ERROR_SHARING_VIOLATION, windows.ERROR_ACCESS_DENIED)
}

// TestTheHarvestHelperReadsPastAHoldOnTheStateFile: the harvest helper's read
// waits out a handle on the state file that shares nothing, the refusal main
// bf622968's gate met at its poll for the settled covers (backlog B898), let
// go by the read at a refusal it heard, never by time, or by the cleanup.
func TestTheHarvestHelperReadsPastAHoldOnTheStateFile(t *testing.T) {
	sharing := func(err error) bool { return errors.Is(err, windows.ERROR_SHARING_VIOLATION) }
	readPastAHold(t, func(t *testing.T, path string) (func(string) ([]byte, error), func()) {
		var h windows.Handle
		// The open waits out a scanner's hold of the file the fixture wrote.
		if err := atomicwrite.ReadWithRetryCtx(t.Context(), func() error {
			name, err := windows.UTF16PtrFromString(path)
			if err == nil {
				h, err = windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		letGo := func() { once.Do(func() { _ = windows.CloseHandle(h) }) }
		t.Cleanup(letGo)
		if _, err := os.ReadFile(path); !sharing(err) {
			t.Fatalf("a read beside a handle that shares nothing = %v, want ERROR_SHARING_VIOLATION: the case holds nothing", err)
		}
		return os.ReadFile, letGo
	}, sharing)
}
