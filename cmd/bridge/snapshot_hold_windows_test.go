//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// processHoldsFile reports whether any handle, this process's included,
// has path open. An exclusive open is the check Windows itself makes when
// TempDir cleanup deletes the file. A path that is not there is not held.
func processHoldsFile(path string) (bool, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		0, // exclusive: a handle already open fails this
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err == nil {
		_ = windows.CloseHandle(h)
		return false, nil
	}
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return true, nil
	}
	if os.IsNotExist(err) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return false, nil
	}
	return false, err
}
