//go:build windows

package main

import (
	"errors"
	"os"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

// TestTheHarvestHelperReadsPastAHoldOnTheStateFile: the harvest helper's read
// waits out a handle on the state file that shares nothing, the sharing
// violation main bf622968's gate met at its poll for the settled covers
// (backlog B898), let go by the read alone at a refusal it heard, never by
// time, and by the cleanup whatever the case did.
func TestTheHarvestHelperReadsPastAHoldOnTheStateFile(t *testing.T) {
	sharing := func(err error) bool { return errors.Is(err, windows.ERROR_SHARING_VIOLATION) }
	readPastAHold(t, func(t *testing.T, path string) (func(string) ([]byte, error), func()) {
		name, err := windows.UTF16PtrFromString(path)
		var h windows.Handle
		if err == nil {
			h, err = windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		}
		if err != nil {
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
