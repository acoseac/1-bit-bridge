package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// serveLockFileName is the file under DataDir on which a running `bridge
// serve` holds an exclusive kernel lock (fsutil.TryLock), from before its
// first write there until runServe returns (backlog B208). The file is
// never removed: only the lock on it means anything, and unlinking a lock
// file while it is held lets a later serve lock a NEW file at the same
// path beside the old holder.
const serveLockFileName = "server.lock"

// serveDataDirHeldError is lockServeDataDir's answer when another `bridge
// serve` holds the data dir's lock: in another process, or in this one
// (the launcher menu and the tests run serve in-process).
type serveDataDirHeldError struct {
	dataDir string
	// pid is the pid server.pid names, 0 when it names none. The holder
	// writes server.pid after it takes the lock, so this is the holder's
	// unless that write failed (it is not fatal).
	pid int
}

func (e *serveDataDirHeldError) Error() string {
	holder := "another bridge serve"
	if e.pid > 0 {
		holder = fmt.Sprintf("another bridge serve (pid %d, from %s)", e.pid, serverPIDFileName)
	}
	return fmt.Sprintf("%s is already running on the data dir %s", holder, e.dataDir)
}

// lockServeDataDir takes the lock that keeps a second `bridge serve` off a
// data dir a live one serves, and returns what releases it. runServe asks
// before its first write to the data dir and before it binds anything: a
// second serve used to run all of its wiring and fail only at the bind,
// and by then its batch coordinator had marked the live bridge's running
// batches interrupted, its exit had removed the live bridge's server.pid
// and rewritten tokens.json, and a second serve on ports of its own never
// failed at all (backlog B208).
//
// It answers a *serveDataDirHeldError when another serve holds the lock,
// which is the one answer that refuses. Any other error means no lock
// could be taken (a filesystem that keeps no locks, a lock file this user
// cannot open): the caller warns and serves without the check, since a
// lock that cannot be taken must not stop a bridge from starting. release
// is never nil, and does nothing when no lock was taken.
//
// A kernel lock, not a lock FILE, which this repo declines for `bridge
// restore` because one left by a crash would block restore just when it is
// needed: the kernel drops this lock with the process however it exits, so
// a supervisor's restart after a SIGKILL finds it free.
func lockServeDataDir(dataDir string) (release func(), err error) {
	release = func() {}
	if err := fsutil.MkdirAll(dataDir, 0o700); err != nil {
		return release, err
	}
	path := filepath.Join(dataDir, serveLockFileName)
	// Run as root over an install another user owns, the file takes the
	// data dir's owner, as every other file serve writes there does: left
	// root's, the service user could not open it again.
	if err := fsutil.Precreate(path, 0o600, path); err != nil && !errors.Is(err, os.ErrExist) {
		return release, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return release, err
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		if errors.Is(err, fsutil.ErrLocked) {
			return release, &serveDataDirHeldError{dataDir: dataDir, pid: recordedServePID(dataDir)}
		}
		return release, err
	}
	return func() {
		// Unlock before the close: on Windows the release a close makes
		// can lag, and a restart in this process would find it held.
		_ = fsutil.Unlock(f)
		_ = f.Close()
	}, nil
}

// recordedServePID is the pid dataDir's server.pid names, or 0.
func recordedServePID(dataDir string) int {
	b, err := os.ReadFile(filepath.Join(dataDir, serverPIDFileName))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}
