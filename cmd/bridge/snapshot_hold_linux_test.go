//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
)

// processHoldsFile reports whether this process has path open. A POSIX
// lock is per process, so it cannot see a handle this process already
// holds; /proc/self/fd can. A path that is not there is not held.
func processHoldsFile(path string) (bool, error) {
	targetInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	// Names only: ReadDir stats each entry, and a descriptor that closes
	// between the listing and the stat fails the whole call (the darwin
	// helper's comment records the EBADF).
	d, err := os.Open("/proc/self/fd")
	if err != nil {
		return false, err
	}
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if _, err := strconv.Atoi(name); err != nil {
			continue
		}
		link, err := os.Readlink(filepath.Join("/proc/self/fd", name))
		if err != nil {
			continue
		}
		info, err := os.Stat(link)
		if err != nil {
			continue
		}
		if os.SameFile(targetInfo, info) {
			return true, nil
		}
	}
	return false, nil
}
